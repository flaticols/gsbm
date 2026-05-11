package gsbmschema

import (
	"testing"
)

// TestValidateTagUniqueness confirms that two fields sharing a tag in
// the same struct are flagged as `tag/duplicate`.
func TestValidateTagUniqueness(t *testing.T) {
	ps, err := ParseSource("p", []string{`
package p

//gsbm:root
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

//gsbm:root
//gsbm:reserved 7
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

//gsbm:root
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

// TestValidateRejectsCustomMarshaler — `bin:"N,custom=Foo"` is plumbed
// through schema/classifier/hash so the append-only policy can guard the
// wire-shape change once codegen learns to dispatch on it. Until then,
// accepting the annotation would silently shift schemaHint + review labels
// with zero wire effect, which is worse than rejecting the input. The
// validator must surface field/custom-not-supported.
func TestValidateRejectsCustomMarshaler(t *testing.T) {
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
	s, _ := BuildSchema(ps, roots)
	issues := Validate(s, ps)
	if !hasIssueCode(issues, "field/custom-not-supported") {
		t.Fatalf("expected field/custom-not-supported, got %v", issues)
	}
}

// TestValidateOptionalComposite — `*[]T` (non-byte), `*map[K]V`, and
// `*[N]T` are rejected because the codegen has no decode path for them.
// `*[]byte` is the explicit exception, supported by both encoder and
// decoder via the spec §5.1 zero-elide rule.
func TestValidateOptionalComposite(t *testing.T) {
	cases := []struct {
		name      string
		field     string
		wantIssue bool
	}{
		{"slice of int", "*[]int64", true},
		{"map", "*map[string]int64", true},
		{"array", "*[4]int64", true},
		{"byte slice exception", "*[]byte", false},
		{"value slice", "[]int64", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := `
package p

//gsbm:root
type Offer struct {
	F ` + tc.field + ` ` + "`bin:\"1\"`" + `
}
`
			ps, err := ParseSource("p", []string{src})
			if err != nil {
				t.Fatal(err)
			}
			roots, _ := Discover(ps)
			s, _ := BuildSchema(ps, roots)
			issues := Validate(s, ps)
			got := hasIssueCode(issues, "field/optional-composite")
			if got != tc.wantIssue {
				t.Fatalf("field %s: want issue=%v, got %v (issues=%v)", tc.field, tc.wantIssue, got, issues)
			}
		})
	}
}

// TestValidateNoCycles — A → B → A cycle MUST be rejected unless one
// of the participating fields carries //gsbm:cycle_break_via_id.
func TestValidateNoCycles(t *testing.T) {
	src := `
package p

//gsbm:root
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

	// Same graph, but A.B carries //gsbm:cycle_break_via_id — accepted.
	srcOK := `
package p

//gsbm:root
type A struct {
	//gsbm:cycle_break_via_id
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

	// Same graph, but using the new `bin:"1,id_ref"` tag option instead of
	// the legacy //gsbm:cycle_break_via_id comment marker. The tag option
	// is the new preferred syntax; both forms must produce the same
	// validator outcome.
	srcIDRef := `
package p

//gsbm:root
type A struct {
	B *B ` + "`bin:\"1,id_ref\"`" + `
}

type B struct {
	A *A ` + "`bin:\"1\"`" + `
}
`
	ps3, err := ParseSource("p", []string{srcIDRef})
	if err != nil {
		t.Fatal(err)
	}
	roots3, _ := Discover(ps3)
	s3, _ := BuildSchema(ps3, roots3)
	issues3 := Validate(s3, ps3)
	if hasIssueCode(issues3, "type/cycle") {
		t.Fatalf("did not expect type/cycle with id_ref tag option, got %v", issues3)
	}
}

// TestValidateIDRefBadTarget — `bin:"N,id_ref"` is only meaningful on a
// pointer-to-struct field. Applying it to a non-pointer field (e.g. a
// `string` or a value struct) must be rejected with tag/bad-id-ref so
// authors don't silently accept a no-op marker.
func TestValidateIDRefBadTarget(t *testing.T) {
	cases := []struct {
		name string
		src  string
	}{
		{
			name: "string",
			src: `
package p

//gsbm:root
type A struct {
	ID string ` + "`bin:\"1,id_ref\"`" + `
}
`,
		},
		{
			name: "value-struct",
			src: `
package p

type B struct {
	ID string ` + "`bin:\"1\"`" + `
}

//gsbm:root
type A struct {
	B B ` + "`bin:\"1,id_ref\"`" + `
}
`,
		},
		{
			name: "slice-of-pointer",
			src: `
package p

type B struct {
	ID string ` + "`bin:\"1\"`" + `
}

//gsbm:root
type A struct {
	B []*B ` + "`bin:\"1,id_ref\"`" + `
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
			_, issues := Discover(ps)
			// Discover and BuildSchema together surface the issue; collect
			// both to cover whichever phase fires first.
			roots, _ := Discover(ps)
			_, more := BuildSchema(ps, roots)
			issues = append(issues, more...)
			if !hasIssueCode(issues, "tag/bad-id-ref") {
				t.Fatalf("expected tag/bad-id-ref, got %v", issues)
			}
		})
	}
}

// TestValidateNoCyclesThroughComposites — cycles routed through nested
// composite types (slice-of-map, map-of-map, slice-of-slice, …) MUST be
// detected. An earlier implementation only stripped the outer `*` and
// `[]` prefixes from shape strings and missed any cycle whose path
// crossed a `map[K]…` or another inner composite.
func TestValidateNoCyclesThroughComposites(t *testing.T) {
	cases := []struct {
		name string
		src  string
	}{
		{
			name: "slice-of-map-of-pointer",
			src: `
package p

//gsbm:root
type A struct {
	Bs []map[string]*B ` + "`bin:\"1\"`" + `
}

type B struct {
	A *A ` + "`bin:\"1\"`" + `
}
`,
		},
		{
			name: "map-of-map-of-struct",
			src: `
package p

//gsbm:root
type A struct {
	Bs map[string]map[string]B ` + "`bin:\"1\"`" + `
}

type B struct {
	A *A ` + "`bin:\"1\"`" + `
}
`,
		},
		{
			name: "array-of-pointer",
			src: `
package p

//gsbm:root
type A struct {
	Bs [4]*B ` + "`bin:\"1\"`" + `
}

type B struct {
	A *A ` + "`bin:\"1\"`" + `
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
			s, _ := BuildSchema(ps, roots)
			issues := Validate(s, ps)
			if !hasIssueCode(issues, "type/cycle") {
				t.Fatalf("expected type/cycle, got %v", issues)
			}
		})
	}
}

// TestValidateOpaqueOptOut — an //gsbm:opaque struct is included as a
// placeholder and is NOT walked into. Field-level issues inside such a
// struct MUST NOT be raised.
func TestValidateOpaqueOptOut(t *testing.T) {
	ps, err := ParseSource("p", []string{`
package p

//gsbm:opaque
type Inner struct {
	NoTag string
}

//gsbm:root
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

// TestValidateRejectsGenerics — generic origins and their instantiations
// cannot be codegen'd: Go does not permit a method body that varies per
// type argument, so neither Box[T] nor Box[int] gets a MarshalGSBM. A
// non-generic parent referencing Box[int] would compile-fail at the call
// site. Validate must surface this as type/generic so the failure is
// caught before codegen runs.
func TestValidateRejectsGenerics(t *testing.T) {
	ps, err := ParseSource("p", []string{`
package p

type Box[T any] struct {
	Value T ` + "`bin:\"1\"`" + `
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
	s, _ := BuildSchema(ps, roots)
	issues := Validate(s, ps)
	if !hasIssueCode(issues, "type/generic") {
		t.Fatalf("expected type/generic, got %v", issues)
	}
}

// TestValidateAcceptsOpaqueGenerics — a generic struct marked //gsbm:opaque
// has handwritten Marshal/Unmarshal/Reset. Go's per-instantiation generic
// methods make Box[int].MarshalGSBM resolve at the parent's call site, so
// the parent's generated code compiles without per-instantiation codegen.
// Validate must NOT flag this as type/generic.
func TestValidateAcceptsOpaqueGenerics(t *testing.T) {
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
	s, _ := BuildSchema(ps, roots)
	issues := Validate(s, ps)
	if hasIssueCode(issues, "type/generic") {
		t.Fatalf("opaque generic must not surface type/generic: %v", issues)
	}
}

// TestRejectIndirectOpaqueGenerics — an opaque generic struct used outside
// the direct-value-field shape is rejected. typeExpr renders named types
// without type arguments, so `*Box[int]`, `[]Box[int]`, `map[K]Box[int]`
// would emit `&Box{}`, `MakeSlice[Box]`, `var vv Box` — none of which
// compile. Discover-time checkSupportedType surfaces type/generic for
// each shape so the failure precedes codegen.
func TestRejectIndirectOpaqueGenerics(t *testing.T) {
	cases := []struct {
		name  string
		field string
	}{
		{"pointer", "B *Box[int]"},
		{"slice", "B []Box[int]"},
		{"map", "B map[string]Box[int]"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := `package p

//gsbm:opaque
type Box[T any] struct {
	Value T
}

//gsbm:root
type Root struct {
	` + tc.field + " `bin:\"1\"`" + `
}
`
			ps, err := ParseSource("p", []string{src})
			if err != nil {
				t.Fatal(err)
			}
			roots, _ := Discover(ps)
			_, bIssues := BuildSchema(ps, roots)
			if !hasIssueCode(bIssues, "type/generic") {
				t.Fatalf("expected type/generic for indirect opaque-generic %q, got %v", tc.field, bIssues)
			}
		})
	}
}

// TestRejectGenericNamedAlias — `type Label[T any] string` instantiated as
// `Label[int]` must be rejected. The codegen path for named-not-struct
// would emit `Label(tmp)` (no type arguments) which Go cannot resolve.
func TestRejectGenericNamedAlias(t *testing.T) {
	ps, err := ParseSource("p", []string{`
package p

type Label[T any] string

//gsbm:root
type Root struct {
	L Label[int] ` + "`bin:\"1\"`" + `
}
`})
	if err != nil {
		t.Fatal(err)
	}
	roots, _ := Discover(ps)
	_, bIssues := BuildSchema(ps, roots)
	if !hasIssueCode(bIssues, "type/generic") {
		t.Fatalf("expected type/generic for Label[int], got %v", bIssues)
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
