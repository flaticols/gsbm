package gsbmschema

import (
	"os"
	"path/filepath"
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

// TestCustomCodecRejectsCycleBreakMarker — `//gsbm:cycle_break_via_id`
// combined with `custom=` must be rejected the same way as the tag-only
// form `bin:"N,id_ref,custom=…"`. The marker lives on the comment, not
// the tag, so ParseFieldTag does not see it; without an explicit check
// in BuildSchema the field gets recorded as both cycle-break and custom,
// and codegen silently picks the id_ref path (CycleBreak takes precedence
// over Custom in emit.go), producing a wire shape that disagrees with
// the schema snapshot's classifier view.
func TestCustomCodecRejectsCycleBreakMarker(t *testing.T) {
	ps, err := ParseSource("p", []string{`
package p

type Target struct {
	ID uint64 ` + "`bin:\"1\"`" + `
}

//gsbm:root
type Root struct {
	//gsbm:cycle_break_via_id
	T *Target ` + "`bin:\"1,custom=TargetCodec\"`" + `
}
`})
	if err != nil {
		t.Fatal(err)
	}
	roots, _ := Discover(ps)
	_, issues := BuildSchema(ps, roots)
	if !hasIssueCode(issues, "tag/parse") {
		t.Fatalf("expected tag/parse for id_ref+custom marker combo, got %v", issues)
	}
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

// TestEmbedFlattenSimple — a value-embedded struct contributes its tagged
// fields to the outer struct's flattened field list. The embedded type
// name is recorded on each promoted field's FlattenedFrom.
func TestEmbedFlattenSimple(t *testing.T) {
	ps, err := ParseSource("p", []string{`
package p

type Base struct {
	Total int64 ` + "`bin:\"1\"`" + `
}

//gsbm:root
type Extended struct {
	Base
	Reason string ` + "`bin:\"2\"`" + `
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
	ext := findStruct(schema, "Extended")
	if ext == nil {
		t.Fatal("Extended missing")
	}
	if len(ext.Fields) != 2 {
		t.Fatalf("expected 2 flattened fields on Extended, got %d: %+v", len(ext.Fields), ext.Fields)
	}
	var total, reason *FieldDecl
	for _, fd := range ext.Fields {
		switch fd.Name {
		case "Total":
			total = fd
		case "Reason":
			reason = fd
		}
	}
	if total == nil || reason == nil {
		t.Fatalf("expected Total+Reason fields, got %+v", ext.Fields)
	}
	if total.Tag != 1 || total.FlattenedFrom != "Base" {
		t.Errorf("Total: tag=%d FlattenedFrom=%q (want tag=1, FlattenedFrom=Base)", total.Tag, total.FlattenedFrom)
	}
	if total.FlattenedFromPointer {
		t.Errorf("Total: expected FlattenedFromPointer=false for value embed")
	}
	if reason.Tag != 2 || reason.FlattenedFrom != "" {
		t.Errorf("Reason: tag=%d FlattenedFrom=%q (want tag=2, direct)", reason.Tag, reason.FlattenedFrom)
	}
}

// TestEmbedFlattenCollision — a tag collision between a direct field and
// an embedded field surfaces as `field/tag-collision` and names both
// sides plus the embedded type.
func TestEmbedFlattenCollision(t *testing.T) {
	ps, err := ParseSource("p", []string{`
package p

type Base struct {
	X int64 ` + "`bin:\"1\"`" + `
}

//gsbm:root
type Outer struct {
	Base
	Y int64 ` + "`bin:\"1\"`" + `
}
`})
	if err != nil {
		t.Fatal(err)
	}
	roots, _ := Discover(ps)
	_, issues := BuildSchema(ps, roots)
	if !hasIssueCode(issues, "field/tag-collision") {
		t.Fatalf("expected field/tag-collision, got %v", issues)
	}
	var hit *Issue
	for i := range issues {
		if issues[i].Code == "field/tag-collision" {
			hit = &issues[i]
			break
		}
	}
	if hit == nil {
		t.Fatal("collision issue missing")
	}
	for _, want := range []string{"X", "Y", "Base"} {
		if !strings.Contains(hit.Message, want) {
			t.Errorf("collision message %q missing %q", hit.Message, want)
		}
	}
}

// TestEmbedFlattenMultiLevel — embedding chains through multiple levels:
// A embeds B, B embeds C; all of C's tagged fields appear on A with the
// full chain captured in FlattenedFrom.
func TestEmbedFlattenMultiLevel(t *testing.T) {
	ps, err := ParseSource("p", []string{`
package p

type C struct {
	CV int64 ` + "`bin:\"1\"`" + `
}

type B struct {
	C
	BV int64 ` + "`bin:\"2\"`" + `
}

//gsbm:root
type A struct {
	B
	AV int64 ` + "`bin:\"3\"`" + `
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
	a := findStruct(schema, "A")
	if a == nil {
		t.Fatal("A missing")
	}
	if len(a.Fields) != 3 {
		t.Fatalf("expected 3 fields on A, got %d: %+v", len(a.Fields), a.Fields)
	}
	wantOrigin := map[string]string{"CV": "B.C", "BV": "B", "AV": ""}
	for _, fd := range a.Fields {
		want, ok := wantOrigin[fd.Name]
		if !ok {
			t.Errorf("unexpected field %s on A", fd.Name)
			continue
		}
		if fd.FlattenedFrom != want {
			t.Errorf("%s FlattenedFrom=%q want %q", fd.Name, fd.FlattenedFrom, want)
		}
	}
}

// TestEmbedFlattenMultiLevelCollision — a tag collision across multiple
// embed levels (B's tagged field collides with C's tagged field through
// the same chain) surfaces as `field/tag-collision`.
func TestEmbedFlattenMultiLevelCollision(t *testing.T) {
	ps, err := ParseSource("p", []string{`
package p

type C struct {
	X int64 ` + "`bin:\"1\"`" + `
}

type B struct {
	C
	Y int64 ` + "`bin:\"1\"`" + `
}

//gsbm:root
type A struct {
	B
	Z int64 ` + "`bin:\"2\"`" + `
}
`})
	if err != nil {
		t.Fatal(err)
	}
	roots, _ := Discover(ps)
	_, issues := BuildSchema(ps, roots)
	if !hasIssueCode(issues, "field/tag-collision") {
		t.Fatalf("expected field/tag-collision across multi-level embed, got %v", issues)
	}
}

// TestEmbedFlattenNonStructRejected — embedding a named primitive (not a
// struct) is rejected with the new `field/anonymous-non-struct` diagnostic
// that directs the user to the named-field rewrite.
func TestEmbedFlattenNonStructRejected(t *testing.T) {
	ps, err := ParseSource("p", []string{`
package p

type Name string

//gsbm:root
type Outer struct {
	Name
	V int64 ` + "`bin:\"1\"`" + `
}
`})
	if err != nil {
		t.Fatal(err)
	}
	roots, _ := Discover(ps)
	_, issues := BuildSchema(ps, roots)
	if !hasIssueCode(issues, "field/anonymous-non-struct") {
		t.Fatalf("expected field/anonymous-non-struct, got %v", issues)
	}
}

// TestEmbedFlattenPointer — pointer-to-struct embeds flatten the same way
// as value embeds; FlattenedFromPointer is set on the promoted fields so
// codegen can emit the nil-check on encode and lazy allocation on decode.
func TestEmbedFlattenPointer(t *testing.T) {
	ps, err := ParseSource("p", []string{`
package p

type Base struct {
	Total int64 ` + "`bin:\"1\"`" + `
}

//gsbm:root
type Outer struct {
	*Base
	Reason string ` + "`bin:\"2\"`" + `
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
	outer := findStruct(schema, "Outer")
	if outer == nil {
		t.Fatal("Outer missing")
	}
	var total *FieldDecl
	for _, fd := range outer.Fields {
		if fd.Name == "Total" {
			total = fd
		}
	}
	if total == nil {
		t.Fatal("Total field not flattened from *Base")
	}
	if total.FlattenedFrom != "Base" || !total.FlattenedFromPointer {
		t.Errorf("Total: FlattenedFrom=%q FlattenedFromPointer=%v (want Base/true)", total.FlattenedFrom, total.FlattenedFromPointer)
	}
}

// TestEmbedFlattenCycleRejected — two structs that pointer-embed each
// other compile fine in Go but would drive the flattening walk into
// unbounded recursion. The validator must detect the cycle and emit
// `field/embed-cycle` rather than hanging.
func TestEmbedFlattenCycleRejected(t *testing.T) {
	ps, err := ParseSource("p", []string{`
package p

//gsbm:root
type A struct {
	*B
	X int64 ` + "`bin:\"1\"`" + `
}

type B struct {
	*A
	Y int64 ` + "`bin:\"2\"`" + `
}
`})
	if err != nil {
		t.Fatal(err)
	}
	roots, _ := Discover(ps)
	_, issues := BuildSchema(ps, roots)
	if !hasIssueCode(issues, "field/embed-cycle") {
		t.Fatalf("expected field/embed-cycle, got %v", issues)
	}
}

// TestEmbedFlattenOpaqueRejected — embedding a //gsbm:opaque type would
// silently bypass its handwritten Marshal/Unmarshal because the flattener
// promotes the inner fields and codegen emits inline encode/decode. The
// validator must reject the embed so the opaque contract is preserved.
func TestEmbedFlattenOpaqueRejected(t *testing.T) {
	ps, err := ParseSource("p", []string{`
package p

//gsbm:opaque
type Base struct {
	Total int64 ` + "`bin:\"1\"`" + `
}

//gsbm:root
type Outer struct {
	Base
	Reason string ` + "`bin:\"2\"`" + `
}
`})
	if err != nil {
		t.Fatal(err)
	}
	roots, _ := Discover(ps)
	_, issues := BuildSchema(ps, roots)
	if !hasIssueCode(issues, "field/anonymous-opaque") {
		t.Fatalf("expected field/anonymous-opaque, got %v", issues)
	}
}

// TestEmbedFlattenGenericRejected — anonymous embedding of a generic
// instantiation compiles in Go but codegen renders named types without
// type arguments, producing invalid output. The validator must reject the
// embed up front.
func TestEmbedFlattenGenericRejected(t *testing.T) {
	ps, err := ParseSource("p", []string{`
package p

type Box[T any] struct {
	V T ` + "`bin:\"1\"`" + `
}

//gsbm:root
type Outer struct {
	Box[int64]
	Reason string ` + "`bin:\"2\"`" + `
}
`})
	if err != nil {
		t.Fatal(err)
	}
	roots, _ := Discover(ps)
	_, issues := BuildSchema(ps, roots)
	if !hasIssueCode(issues, "field/anonymous-generic") {
		t.Fatalf("expected field/anonymous-generic, got %v", issues)
	}
}

// TestEmbedFlattenReservedTagsMerged — //gsbm:reserved on an embedded type
// extends the outer struct's reserved set; declaring a field on a tag the
// base reserved must fire `tag/reserved` so flattening preserves the
// append-only guarantee.
func TestEmbedFlattenReservedTagsMerged(t *testing.T) {
	ps, err := ParseSource("p", []string{`
package p

//gsbm:reserved 5
type Base struct {
	Total int64 ` + "`bin:\"1\"`" + `
}

//gsbm:root
type Outer struct {
	Base
	Reason string ` + "`bin:\"5\"`" + `
}
`})
	if err != nil {
		t.Fatal(err)
	}
	roots, _ := Discover(ps)
	schema, issues := BuildSchema(ps, roots)
	issues = append(issues, Validate(schema, ps)...)
	if !hasIssueCode(issues, "tag/reserved") {
		t.Fatalf("expected tag/reserved from merged embed reservation, got %v", issues)
	}
}

// TestEmbedFlattenCrossPackageUnexportedField — codegen emits the
// explicit dotted access path `v.<Embed>.<Field>` for promoted fields. If
// the leaf field is unexported and lives in a different package than the
// outer struct, the generated file in the outer's package will not
// compile. Discover surfaces this as `field/anonymous-unexported-field`
// before codegen runs. Same-package unexported is fine and must not
// trigger the diagnostic.
func TestEmbedFlattenCrossPackageUnexportedField(t *testing.T) {
	root := t.TempDir()
	if err := writeMod(root, "example.com/embed\n\ngo 1.26\n"); err != nil {
		t.Fatal(err)
	}
	dirA := filepath.Join(root, "a")
	dirB := filepath.Join(root, "b")
	if err := os.MkdirAll(dirA, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dirB, 0o755); err != nil {
		t.Fatal(err)
	}
	srcB := []byte(`package b

type Base struct {
	total int64 ` + "`bin:\"1\"`" + `
}
`)
	srcA := []byte(`package a

import "example.com/embed/b"

//gsbm:root
type Outer struct {
	b.Base
	Reason string ` + "`bin:\"2\"`" + `
}
`)
	if err := os.WriteFile(filepath.Join(dirA, "a.go"), srcA, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dirB, "b.go"), srcB, 0o644); err != nil {
		t.Fatal(err)
	}
	ps, err := LoadFromDirs([]string{dirA, dirB})
	if err != nil {
		t.Fatalf("LoadFromDirs: %v", err)
	}
	roots, issues := Discover(ps)
	_, more := BuildSchema(ps, roots)
	issues = append(issues, more...)
	if !hasIssueCode(issues, "field/anonymous-unexported-field") {
		t.Fatalf("expected field/anonymous-unexported-field, got %v", issues)
	}

	// Sanity check: same-package unexported promoted field compiles fine
	// because codegen emits into the field's own package; the
	// cross-package diagnostic must NOT fire here.
	ps2, err := ParseSource("p", []string{`
package p

type Base struct {
	total int64 ` + "`bin:\"1\"`" + `
}

//gsbm:root
type Outer struct {
	Base
	Reason string ` + "`bin:\"2\"`" + `
}
`})
	if err != nil {
		t.Fatal(err)
	}
	roots2, issues2 := Discover(ps2)
	_, more2 := BuildSchema(ps2, roots2)
	issues2 = append(issues2, more2...)
	if hasIssueCode(issues2, "field/anonymous-unexported-field") {
		t.Fatalf("same-package unexported promoted field must not be flagged, got %v", issues2)
	}
}

// TestEmbedFlattenCrossPackageUnexportedFieldType — even when the
// promoted field's name is exported, codegen still references the field's
// type when emitting the access path's RHS. An unexported named type in a
// foreign package cannot be referenced from the outer's package; Discover
// surfaces this as `field/anonymous-unexported-type` before codegen runs.
func TestEmbedFlattenCrossPackageUnexportedFieldType(t *testing.T) {
	root := t.TempDir()
	if err := writeMod(root, "example.com/embed\n\ngo 1.26\n"); err != nil {
		t.Fatal(err)
	}
	dirA := filepath.Join(root, "a")
	dirB := filepath.Join(root, "b")
	if err := os.MkdirAll(dirA, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dirB, 0o755); err != nil {
		t.Fatal(err)
	}
	srcB := []byte(`package b

type secret int64

type Base struct {
	Total secret ` + "`bin:\"1\"`" + `
}
`)
	srcA := []byte(`package a

import "example.com/embed/b"

//gsbm:root
type Outer struct {
	b.Base
	Reason string ` + "`bin:\"2\"`" + `
}
`)
	if err := os.WriteFile(filepath.Join(dirA, "a.go"), srcA, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dirB, "b.go"), srcB, 0o644); err != nil {
		t.Fatal(err)
	}
	ps, err := LoadFromDirs([]string{dirA, dirB})
	if err != nil {
		t.Fatalf("LoadFromDirs: %v", err)
	}
	roots, issues := Discover(ps)
	_, more := BuildSchema(ps, roots)
	issues = append(issues, more...)
	if !hasIssueCode(issues, "field/anonymous-unexported-type") {
		t.Fatalf("expected field/anonymous-unexported-type, got %v", issues)
	}
}

// TestEmbedFlattenCrossPackageUnexportedIntermediate — an intermediate
// anonymous segment whose type is unexported in a foreign package is
// embedded by the foreign Base internally; the outer struct can still
// embed Base legally, but codegen's full chain expansion would render
// `v.Base.hidden.Total` which can't compile in the outer's package.
// Discover surfaces this as `field/anonymous-unexported-type` keyed on
// the intermediate segment rather than the leaf field's declared type.
func TestEmbedFlattenCrossPackageUnexportedIntermediate(t *testing.T) {
	root := t.TempDir()
	if err := writeMod(root, "example.com/embed\n\ngo 1.26\n"); err != nil {
		t.Fatal(err)
	}
	dirA := filepath.Join(root, "a")
	dirB := filepath.Join(root, "b")
	if err := os.MkdirAll(dirA, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dirB, 0o755); err != nil {
		t.Fatal(err)
	}
	srcB := []byte(`package b

type hidden struct {
	Total int64 ` + "`bin:\"1\"`" + `
}

type Base struct {
	hidden
}
`)
	srcA := []byte(`package a

import "example.com/embed/b"

//gsbm:root
type Outer struct {
	b.Base
	Reason string ` + "`bin:\"2\"`" + `
}
`)
	if err := os.WriteFile(filepath.Join(dirA, "a.go"), srcA, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dirB, "b.go"), srcB, 0o644); err != nil {
		t.Fatal(err)
	}
	ps, err := LoadFromDirs([]string{dirA, dirB})
	if err != nil {
		t.Fatalf("LoadFromDirs: %v", err)
	}
	roots, issues := Discover(ps)
	_, more := BuildSchema(ps, roots)
	issues = append(issues, more...)
	if !hasIssueCode(issues, "field/anonymous-unexported-type") {
		t.Fatalf("expected field/anonymous-unexported-type for intermediate segment, got %v", issues)
	}
}

// TestEmbedFlattenCrossPackageUnexportedCompositeType — even when the
// promoted leaf field's type is not directly a *types.Named, codegen
// renders the full composite (`*pkg.secret`, `[]pkg.secret`, `map[string]pkg.secret`)
// and references the unexported foreign name for both encode walks and
// decode allocations. Discover must reject each composite form with
// `field/anonymous-unexported-type` so the build break surfaces before
// codegen runs.
func TestEmbedFlattenCrossPackageUnexportedCompositeType(t *testing.T) {
	for _, tc := range []struct {
		name      string
		fieldDecl string
	}{
		{name: "pointer", fieldDecl: "Total *secret `bin:\"1\"`"},
		{name: "slice", fieldDecl: "Items []secret `bin:\"1\"`"},
		{name: "map_value", fieldDecl: "Items map[string]secret `bin:\"1\"`"},
		{name: "map_key", fieldDecl: "Items map[secret]int64 `bin:\"1\"`"},
		{name: "array", fieldDecl: "Items [3]secret `bin:\"1\"`"},
		// Named slice whose underlying element is foreign+unexported.
		// Codegen unwraps the named slice to `[]secret` and emits
		// `gsbm.MakeSlice[b.secret]` / `b.secret` element refs in the
		// outer's package — must be caught by the validator.
		{name: "named_slice_unexported_elem", fieldDecl: "Items ExportedList `bin:\"1\"`"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			if err := writeMod(root, "example.com/embed\n\ngo 1.26\n"); err != nil {
				t.Fatal(err)
			}
			dirA := filepath.Join(root, "a")
			dirB := filepath.Join(root, "b")
			if err := os.MkdirAll(dirA, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(dirB, 0o755); err != nil {
				t.Fatal(err)
			}
			srcB := []byte(`package b

type secret struct {
	V int64 ` + "`bin:\"1\"`" + `
}

type ExportedList []secret

type Base struct {
	` + tc.fieldDecl + `
}
`)
			srcA := []byte(`package a

import "example.com/embed/b"

//gsbm:root
type Outer struct {
	b.Base
	Reason string ` + "`bin:\"2\"`" + `
}
`)
			if err := os.WriteFile(filepath.Join(dirA, "a.go"), srcA, 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dirB, "b.go"), srcB, 0o644); err != nil {
				t.Fatal(err)
			}
			ps, err := LoadFromDirs([]string{dirA, dirB})
			if err != nil {
				t.Fatalf("LoadFromDirs: %v", err)
			}
			roots, issues := Discover(ps)
			_, more := BuildSchema(ps, roots)
			issues = append(issues, more...)
			if !hasIssueCode(issues, "field/anonymous-unexported-type") {
				t.Fatalf("expected field/anonymous-unexported-type for composite %s, got %v", tc.name, issues)
			}
		})
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
