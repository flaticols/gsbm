package gsbmschema

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeMod writes a go.mod under root so LoadFromDirs has an anchor for
// cross-package discovery in the hermetic two-dir tests below.
func writeMod(root, body string) error {
	return os.WriteFile(filepath.Join(root, "go.mod"), []byte("module "+body), 0o644)
}

// findIssueByCode returns the first issue with the given code, or nil.
func findIssueByCode(issues []Issue, code string) *Issue {
	for i := range issues {
		if issues[i].Code == code {
			return &issues[i]
		}
	}
	return nil
}

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

// TestValidateNamedPrimitiveMapKey — spec §5.3 admits named types whose
// underlying type is a supported primitive (string, bool, signed/unsigned
// int, uintptr) as map keys. The validator MUST accept them. Floats
// remain rejected even when wrapped in a named type because primitive
// map-keys must hash deterministically.
func TestValidateNamedPrimitiveMapKey(t *testing.T) {
	cases := []struct {
		name      string
		decl      string
		field     string
		wantIssue bool
	}{
		{"named-string", "type Code string", "map[Code]int64", false},
		{"named-int64", "type Severity int64", "map[Severity]int64", false},
		{"named-int8", "type Sev int8", "map[Sev]int64", false},
		{"named-uint16", "type Bucket uint16", "map[Bucket]int64", false},
		{"named-bool", "type Flag bool", "map[Flag]int64", false},
		{"named-float64", "type Rate float64", "map[Rate]int64", true},
		{"named-float32", "type Rate float32", "map[Rate]int64", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := `
package p

` + tc.decl + `

//gsbm:root
type Offer struct {
	M ` + tc.field + ` ` + "`bin:\"1\"`" + `
}
`
			ps, err := ParseSource("p", []string{src})
			if err != nil {
				t.Fatal(err)
			}
			roots, _ := Discover(ps)
			s, _ := BuildSchema(ps, roots)
			issues := Validate(s, ps)
			got := hasIssueCode(issues, "map/bad-key")
			if got != tc.wantIssue {
				t.Fatalf("field %s: want map/bad-key=%v, got %v (issues=%v)", tc.field, tc.wantIssue, got, issues)
			}
		})
	}
}

// TestValidateAcceptsCustomMarshaler — `bin:"N,custom=Foo"` opts a field
// out of normal schema traversal: the codec declares its own wire type
// and the validator records the annotation without descending into the
// field's Go type. No `field/custom-not-supported`, no `tag/missing` for
// the underlying type's private fields, no `type/external` for an
// out-of-input package.
func TestValidateAcceptsCustomMarshaler(t *testing.T) {
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
	if hasIssueCode(issues, "field/custom-not-supported") {
		t.Fatalf("unexpected field/custom-not-supported, got %v", issues)
	}
}

// TestValidateCustomMarshalerSkipsExternalTypeTraversal — a field whose
// Go type is `time.Time` (declared outside the schema input) MUST NOT
// surface `tag/missing` (for private fields like `wall`/`ext`/`loc`) or
// `type/external` when annotated with `custom=Name`. The codec replaces
// schema traversal for the field entirely.
func TestValidateCustomMarshalerSkipsExternalTypeTraversal(t *testing.T) {
	ps, err := ParseSource("p", []string{`
package p

import "time"

//gsbm:root
type Event struct {
	ID        uint64    ` + "`bin:\"1\"`" + `
	CreatedAt time.Time ` + "`bin:\"2,custom=Time\"`" + `
}
`})
	if err != nil {
		t.Fatal(err)
	}
	roots, issues := Discover(ps)
	s, bissues := BuildSchema(ps, roots)
	issues = append(issues, bissues...)
	issues = append(issues, Validate(s, ps)...)
	for _, code := range []string{"tag/missing", "type/external", "type/unsupported", "field/custom-not-supported"} {
		if hasIssueCode(issues, code) {
			t.Fatalf("unexpected %s for custom-codec field on time.Time: %v", code, issues)
		}
	}
	// The schema must record the codec name on the field so the
	// classifier can flag transitions and codegen can dispatch.
	var found bool
	for _, sd := range s.Structs {
		if sd.Type.Name != "Event" {
			continue
		}
		for _, fd := range sd.Fields {
			if fd.Name == "CreatedAt" && fd.Custom == "Time" {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("expected Event.CreatedAt to carry Custom=Time, schema=%+v", s)
	}
}

// TestValidateCustomMarshalerPointerOptional — `*time.Time` with a
// custom codec must surface Optional=true so the codegen wraps the
// codec body in the standard LENGTH_DELIM+presence-byte envelope
// (spec §5.1), and must NOT descend into the pointer target's type.
func TestValidateCustomMarshalerPointerOptional(t *testing.T) {
	ps, err := ParseSource("p", []string{`
package p

import "time"

//gsbm:root
type Event struct {
	ID         uint64     ` + "`bin:\"1\"`" + `
	OptionalAt *time.Time ` + "`bin:\"2,custom=Time\"`" + `
}
`})
	if err != nil {
		t.Fatal(err)
	}
	roots, _ := Discover(ps)
	s, _ := BuildSchema(ps, roots)
	issues := Validate(s, ps)
	for _, code := range []string{"tag/missing", "type/external", "type/unsupported"} {
		if hasIssueCode(issues, code) {
			t.Fatalf("unexpected %s for *time.Time custom-codec field: %v", code, issues)
		}
	}
	var found bool
	for _, sd := range s.Structs {
		if sd.Type.Name != "Event" {
			continue
		}
		for _, fd := range sd.Fields {
			if fd.Name == "OptionalAt" {
				if fd.Custom != "Time" {
					t.Fatalf("expected Custom=Time, got %q", fd.Custom)
				}
				if !fd.Optional {
					t.Fatalf("expected Optional=true for *time.Time custom-codec field")
				}
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("OptionalAt not in schema")
	}
}

// TestValidateCustomCompositeRejected — a `custom=Name` tag applies to a
// single value of the codec's Go type. Putting `custom=` on a slice/map
// would miscompile the generated code (codec encode function takes a
// scalar, not a composite). The validator MUST surface this as
// field/custom-composite instead of letting `go build` of the generated
// file fail with a confusing type mismatch.
func TestValidateCustomCompositeRejected(t *testing.T) {
	cases := []struct {
		name      string
		decls     string
		field     string
		wantIssue bool
	}{
		{"slice of time", "", "[]time.Time", true},
		{"map of time", "", "map[string]time.Time", true},
		{"array of time", "", "[4]time.Time", true},
		{"named slice alias", "type Times []time.Time\n", "Times", true},
		{"named map alias", "type EventMap map[string]time.Time\n", "EventMap", true},
		{"named array alias", "type Four [4]time.Time\n", "Four", true},
		{"pointer to named slice alias", "type Times []time.Time\n", "*Times", true},
		{"single value", "", "time.Time", false},
		{"pointer single value", "", "*time.Time", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := `
package p

import "time"

` + tc.decls + `
//gsbm:root
type Event struct {
	F ` + tc.field + ` ` + "`bin:\"1,custom=Time\"`" + `
}
`
			ps, err := ParseSource("p", []string{src})
			if err != nil {
				t.Fatal(err)
			}
			roots, dIssues := Discover(ps)
			s, bIssues := BuildSchema(ps, roots)
			vIssues := Validate(s, ps)
			issues := append(append(append([]Issue{}, dIssues...), bIssues...), vIssues...)
			got := hasIssueCode(issues, "field/custom-composite")
			if got != tc.wantIssue {
				t.Fatalf("field %s: want issue=%v, got %v (issues=%v)", tc.field, tc.wantIssue, got, issues)
			}
		})
	}
}

// TestValidateCustomCodecIsCycleBreak — a custom-codec field hands wire
// shape to the codec and never structurally descends into the field's Go
// type, so it functions as a cycle break exactly like //gsbm:cycle_break_via_id.
// The cycle detector MUST NOT flag a graph whose only structural loop is
// routed through a custom-codec field.
func TestValidateCustomCodecIsCycleBreak(t *testing.T) {
	// A→B→A graph where the A→B edge carries a custom codec. The codec is
	// responsible for serializing B-shaped data without re-entering the
	// schema walker. Discover does not descend into the custom field's
	// Go type, so the validator must agree the cycle is broken.
	src := `
package p

//gsbm:root
type A struct {
	ID uint64 ` + "`bin:\"1\"`" + `
	B  B      ` + "`bin:\"2,custom=BCodec\"`" + `
}

type B struct {
	ID uint64 ` + "`bin:\"1\"`" + `
	A  *A     ` + "`bin:\"2\"`" + `
}
`
	ps, err := ParseSource("p", []string{src})
	if err != nil {
		t.Fatal(err)
	}
	roots, _ := Discover(ps)
	s, _ := BuildSchema(ps, roots)
	issues := Validate(s, ps)
	if hasIssueCode(issues, "type/cycle") {
		t.Fatalf("custom-codec field must break the cycle (codec replaces traversal), got %v", issues)
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
// pointer-to-struct field. Applying it (via tag option or legacy comment
// marker) to a non-pointer field (e.g. a `string` or a value struct) must
// be rejected with tag/bad-id-ref so authors don't silently accept a
// no-op marker that would later panic in codegen via a nil pointer deref
// of the (missing) target struct.
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
		{
			name: "comment-marker-on-string",
			src: `
package p

//gsbm:root
type A struct {
	//gsbm:cycle_break_via_id
	ID string ` + "`bin:\"1\"`" + `
}
`,
		},
		{
			name: "comment-marker-on-value-struct",
			src: `
package p

type B struct {
	ID string ` + "`bin:\"1\"`" + `
}

//gsbm:root
type A struct {
	//gsbm:cycle_break_via_id
	B B ` + "`bin:\"1\"`" + `
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
			roots, issues := Discover(ps)
			_, more := BuildSchema(ps, roots)
			issues = append(issues, more...)
			if !hasIssueCode(issues, "tag/bad-id-ref") {
				t.Fatalf("expected tag/bad-id-ref, got %v", issues)
			}
		})
	}
}

// TestValidateIDRefMissingIDTag — codegen reads `v.Ref.<idName>` from the
// target struct, so the target MUST have a bin:"1" field that resolves
// to a scalar type. Catching this at discover time gives the author the
// stable `idref/missing-id-tag` diagnostic instead of a late codegen error.
func TestValidateIDRefMissingIDTag(t *testing.T) {
	cases := []struct {
		name string
		src  string
	}{
		{
			name: "no-bin-1",
			src: `
package p

type B struct {
	Name string ` + "`bin:\"2\"`" + `
}

//gsbm:root
type A struct {
	Ref *B ` + "`bin:\"1,id_ref\"`" + `
}
`,
		},
		{
			name: "bin-1-is-bool",
			src: `
package p

type B struct {
	Flag bool ` + "`bin:\"1\"`" + `
}

//gsbm:root
type A struct {
	Ref *B ` + "`bin:\"1,id_ref\"`" + `
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
			roots, issues := Discover(ps)
			_, more := BuildSchema(ps, roots)
			issues = append(issues, more...)
			if !hasIssueCode(issues, "idref/missing-id-tag") {
				t.Fatalf("expected idref/missing-id-tag, got %v", issues)
			}
		})
	}
}

// TestValidateIDRefCrossPackageUnexportedID — codegen emits
// `v.Ref.<idName>` for id_ref fields. If the target's bin:"1" field is
// unexported and lives in a different package, the generated code would
// not compile. Discover surfaces this as `idref/unexported-id-field`
// before codegen runs. Same-package unexported is fine and must not
// trigger the diagnostic.
func TestValidateIDRefCrossPackageUnexportedID(t *testing.T) {
	root := t.TempDir()
	if err := writeMod(root, "example.com/idref\n\ngo 1.26\n"); err != nil {
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

type Target struct {
	id string ` + "`bin:\"1\"`" + `
}
`)
	srcA := []byte(`package a

import "example.com/idref/b"

//gsbm:root
type Holder struct {
	Ref *b.Target ` + "`bin:\"1,id_ref\"`" + `
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
	if !hasIssueCode(issues, "idref/unexported-id-field") {
		t.Fatalf("expected idref/unexported-id-field, got %v", issues)
	}

	// Sanity check: same-package unexported bin:"1" must NOT trigger the
	// cross-package diagnostic. The generated `v.Ref.id` is valid Go when
	// the codegen emits into the target's own package.
	ps2, err := ParseSource("p", []string{`
package p

type Target struct {
	id string ` + "`bin:\"1\"`" + `
}

//gsbm:root
type Holder struct {
	Ref *Target ` + "`bin:\"1,id_ref\"`" + `
}
`})
	if err != nil {
		t.Fatal(err)
	}
	roots2, issues2 := Discover(ps2)
	_, more2 := BuildSchema(ps2, roots2)
	issues2 = append(issues2, more2...)
	if hasIssueCode(issues2, "idref/unexported-id-field") {
		t.Fatalf("same-package unexported id field must not be flagged, got %v", issues2)
	}
}

// TestValidateIDRefCrossPackageUnexportedIDType — even when the
// target's bin:"1" field name is exported, codegen still emits the
// field's TYPE via typeExpr (for Reset zeroing or id_ref decode
// conversion of named non-struct types). When that type is itself an
// unexported named type in a foreign package, the emitted
// `<alias>.<unexported>` is uncompilable. Discover must surface this
// as `idref/unexported-id-type` before codegen runs. Same-package is
// fine and must not trigger the diagnostic.
func TestValidateIDRefCrossPackageUnexportedIDType(t *testing.T) {
	root := t.TempDir()
	if err := writeMod(root, "example.com/idref\n\ngo 1.26\n"); err != nil {
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

type id string

type Target struct {
	ID id ` + "`bin:\"1\"`" + `
}
`)
	srcA := []byte(`package a

import "example.com/idref/b"

//gsbm:root
type Holder struct {
	Ref *b.Target ` + "`bin:\"1,id_ref\"`" + `
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
	if !hasIssueCode(issues, "idref/unexported-id-type") {
		t.Fatalf("expected idref/unexported-id-type, got %v", issues)
	}

	// Sanity check: same-package unexported named ID type compiles
	// (no package qualifier in the emitted conversion). Must NOT
	// trigger the cross-package diagnostic.
	ps2, err := ParseSource("p", []string{`
package p

type id string

type Target struct {
	ID id ` + "`bin:\"1\"`" + `
}

//gsbm:root
type Holder struct {
	Ref *Target ` + "`bin:\"1,id_ref\"`" + `
}
`})
	if err != nil {
		t.Fatal(err)
	}
	roots2, issues2 := Discover(ps2)
	_, more2 := BuildSchema(ps2, roots2)
	issues2 = append(issues2, more2...)
	if hasIssueCode(issues2, "idref/unexported-id-type") {
		t.Fatalf("same-package unexported id type must not be flagged, got %v", issues2)
	}
}

// TestDiscoverIDRefWireMatchesTargetIDField — the on-wire payload of an
// id_ref field is the target's bin:"1" field as a leaf scalar; the
// snapshot's recorded Wire MUST reflect that scalar's wire type so a
// later change to the target's ID type registers as `field/wire-changed`
// in the classifier (even when the target is opaque and contributes no
// field-level snapshot of its own).
func TestDiscoverIDRefWireMatchesTargetIDField(t *testing.T) {
	cases := []struct {
		name     string
		idType   string
		wantWire string
		wantType string
	}{
		{"string-id", "string", WireLengthDelim, "test/p.Target/id:string"},
		{"int64-id", "int64", WireVarint, "test/p.Target/id:int64"},
		{"bytes-id", "[]byte", WireLengthDelim, "test/p.Target/id:[]byte"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := `
package p

//gsbm:opaque
type Target struct {
	ID ` + tc.idType + ` ` + "`bin:\"1\"`" + `
}

//gsbm:root
type Holder struct {
	Ref *Target ` + "`bin:\"1,id_ref\"`" + `
}
`
			ps, err := ParseSource("p", []string{src})
			if err != nil {
				t.Fatal(err)
			}
			roots, issues := Discover(ps)
			s, more := BuildSchema(ps, roots)
			issues = append(issues, more...)
			if len(issues) != 0 {
				t.Fatalf("unexpected issues: %v", issues)
			}
			var gotWire, gotType string
			for _, sd := range s.Structs {
				if sd.Type.Name != "Holder" {
					continue
				}
				for _, fd := range sd.Fields {
					if fd.Name == "Ref" {
						gotWire = fd.Wire
						gotType = fd.Type
					}
				}
			}
			if gotWire != tc.wantWire {
				t.Fatalf("Holder.Ref Wire = %q, want %q", gotWire, tc.wantWire)
			}
			if gotType != tc.wantType {
				t.Fatalf("Holder.Ref Type = %q, want %q", gotType, tc.wantType)
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

// TestValidateCycleDiagnosticShape — a 3-node cycle A→B→C→A without any
// break marker must produce a diagnostic that names the cycle path,
// surfaces a suggested break candidate, and mentions both the
// `bin:"N,id_ref"` tag option and the //gsbm:cycle_break_via_id comment
// so authors see both ways to fix the issue.
func TestValidateCycleDiagnosticShape(t *testing.T) {
	src := `
package p

//gsbm:root
type A struct {
	B *B ` + "`bin:\"1\"`" + `
}

type B struct {
	C *C ` + "`bin:\"1\"`" + `
}

type C struct {
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
	issue := findIssueByCode(issues, "type/cycle")
	if issue == nil {
		t.Fatalf("expected type/cycle, got %v", issues)
	}
	msg := issue.Message
	for _, want := range []string{
		"A.B",
		"B.C",
		"C.A",
		"suggested break:",
		"id_ref",
		"//gsbm:cycle_break_via_id",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("expected diagnostic to contain %q, got %q", want, msg)
		}
	}
}

// TestValidateCycleLowestTagWins — when the cycle's fields have distinct
// tags, the suggested break must be the field with the lowest tag. The
// lowest-tag pick is deterministic and conventionally identifies the
// "anchor" field that authors expect to encode as an ID reference.
func TestValidateCycleLowestTagWins(t *testing.T) {
	src := `
package p

//gsbm:root
type A struct {
	B *B ` + "`bin:\"5\"`" + `
}

type B struct {
	C *C ` + "`bin:\"3\"`" + `
}

type C struct {
	D *D ` + "`bin:\"7\"`" + `
}

type D struct {
	E *E ` + "`bin:\"2\"`" + `
}

type E struct {
	A *A ` + "`bin:\"4\"`" + `
}
`
	ps, err := ParseSource("p", []string{src})
	if err != nil {
		t.Fatal(err)
	}
	roots, _ := Discover(ps)
	s, _ := BuildSchema(ps, roots)
	issues := Validate(s, ps)
	issue := findIssueByCode(issues, "type/cycle")
	if issue == nil {
		t.Fatalf("expected type/cycle, got %v", issues)
	}
	msg := issue.Message
	if !strings.Contains(msg, "suggested break: D.E (tag 2)") {
		t.Errorf("expected suggested break D.E (tag 2), got %q", msg)
	}
	if !strings.Contains(msg, `bin:"2,id_ref"`) {
		t.Errorf("expected diagnostic to include the bin:\"2,id_ref\" suggestion, got %q", msg)
	}
}

// TestValidateCyclePreviousIsRecommended — when the lowest-tag field
// along the cycle is named "Previous", the diagnostic must surface it as
// the recommended break candidate. This is the canonical
// previous/parent-link pattern from the issue.
func TestValidateCyclePreviousIsRecommended(t *testing.T) {
	src := `
package p

//gsbm:root
type Item struct {
	Label    string ` + "`bin:\"2\"`" + `
	Previous *Item  ` + "`bin:\"1\"`" + `
}
`
	ps, err := ParseSource("p", []string{src})
	if err != nil {
		t.Fatal(err)
	}
	roots, _ := Discover(ps)
	s, _ := BuildSchema(ps, roots)
	issues := Validate(s, ps)
	issue := findIssueByCode(issues, "type/cycle")
	if issue == nil {
		t.Fatalf("expected type/cycle, got %v", issues)
	}
	msg := issue.Message
	if !strings.Contains(msg, "suggested break: Item.Previous (tag 1)") {
		t.Errorf("expected Item.Previous to be the recommended candidate, got %q", msg)
	}
}

// TestValidateCycleTieBreakByName — when two cycle fields share the
// lowest tag, the diagnostic prefers the one whose name matches the
// Previous/Parent/Ref heuristic. This keeps the suggestion stable and
// aligned with author intent in the common case where every cycle field
// is at tag 1.
func TestValidateCycleTieBreakByName(t *testing.T) {
	src := `
package p

//gsbm:root
type A struct {
	B *B ` + "`bin:\"1\"`" + `
}

type B struct {
	Parent *A ` + "`bin:\"1\"`" + `
}
`
	ps, err := ParseSource("p", []string{src})
	if err != nil {
		t.Fatal(err)
	}
	roots, _ := Discover(ps)
	s, _ := BuildSchema(ps, roots)
	issues := Validate(s, ps)
	issue := findIssueByCode(issues, "type/cycle")
	if issue == nil {
		t.Fatalf("expected type/cycle, got %v", issues)
	}
	msg := issue.Message
	if !strings.Contains(msg, "suggested break: B.Parent (tag 1)") {
		t.Errorf("expected B.Parent (tag 1) to win the tie-break, got %q", msg)
	}
}

// TestIsCycleBreakName — the heuristic identifies conventional anchor
// fields (Previous/Parent and *-Ref suffixes) without sweeping in
// unrelated identifiers containing the substring "ref". A 2-node cycle
// with a tie at the lowest tag is the simplest exercise of the heuristic.
func TestIsCycleBreakName(t *testing.T) {
	for _, n := range []string{
		"Previous", "previous", "PreviousID",
		"Parent", "ParentRef", "BackRef", "ref", "Ref",
	} {
		if !isCycleBreakName(n) {
			t.Errorf("expected %q to match the cycle-break heuristic", n)
		}
	}
	for _, n := range []string{
		"Reference", "Preference", "RefreshToken", "Refactor",
		"Body", "Label", "ID",
	} {
		if isCycleBreakName(n) {
			t.Errorf("%q must not match the cycle-break heuristic", n)
		}
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
