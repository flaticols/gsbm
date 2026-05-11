package gsbmcodegen_test

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"go.flaticols.dev/gsbm/tools/gsbmcodegen"
	"go.flaticols.dev/gsbm/tools/gsbmschema"
)

// fixtureDir returns the absolute path of tools/gsbmcodegen/fixtures/sample
// based on the test source location, so the test runs regardless of cwd.
func fixtureDir(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Join(filepath.Dir(thisFile), "fixtures", "sample")
}

// loadFixture wraps gsbmschema.LoadFromDirs with a t.Fatalf on error.
// LoadFromDirs already skips _test.go and _gsbm{,_arena}.go siblings.
func loadFixture(t *testing.T, dir string) *gsbmschema.PackageSet {
	t.Helper()
	ps, err := gsbmschema.LoadFromDirs([]string{dir})
	if err != nil {
		t.Fatalf("LoadFromDirs(%s): %v", dir, err)
	}
	return ps
}

// TestGoldenSample asserts every committed <type>_gsbm.go is byte-identical
// to what Generate produces from the live fixture. This is the contract
// that lets us hand-edit the fixture and regenerate without surprises.
func TestGoldenSample(t *testing.T) {
	dir := fixtureDir(t)
	ps := loadFixture(t, dir)
	res := gsbmschema.Analyze(ps)
	if len(res.Issues) > 0 {
		t.Fatalf("schema issues: %s", gsbmschema.FormatIssues(res.Issues))
	}
	files, err := gsbmcodegen.Generate(ps, res.Schema)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("no files generated")
	}
	for _, gf := range files {
		want, err := os.ReadFile(gf.Path)
		if err != nil {
			t.Errorf("%s: missing committed golden — write the file then re-run: %v", gf.Path, err)
			t.Logf("--- generated %s ---\n%s", gf.Path, gf.Contents)
			continue
		}
		if string(want) != string(gf.Contents) {
			t.Errorf("%s: drifted from golden", gf.Path)
			t.Logf("--- generated ---\n%s", gf.Contents)
			t.Logf("--- golden ---\n%s", want)
		}
	}
}

// TestGoldenSampleArena asserts every committed <root>_gsbm_arena.go is
// byte-identical to what GenerateArena produces. The arena generator
// emits per-root helpers only — non-root structs (Customer, Item, Total)
// must NOT appear in the output.
func TestGoldenSampleArena(t *testing.T) {
	dir := fixtureDir(t)
	ps := loadFixture(t, dir)
	res := gsbmschema.Analyze(ps)
	if len(res.Issues) > 0 {
		t.Fatalf("schema issues: %s", gsbmschema.FormatIssues(res.Issues))
	}
	files, err := gsbmcodegen.GenerateArena(ps, res.Schema)
	if err != nil {
		t.Fatalf("generate arena: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("no arena files generated")
	}
	// Order and Renamed are the //gsbm:root types in the fixture.
	wantRoots := map[string]bool{"Order": true, "Renamed": true}
	if len(files) != len(wantRoots) {
		t.Errorf("expected %d arena files (one per //gsbm:root); got %d", len(wantRoots), len(files))
	}
	for _, gf := range files {
		if !wantRoots[gf.TypeName] {
			t.Errorf("unexpected arena file for non-root %s", gf.TypeName)
		}
		delete(wantRoots, gf.TypeName)
	}
	for missing := range wantRoots {
		t.Errorf("missing arena file for root %s", missing)
	}
	for _, gf := range files {
		want, err := os.ReadFile(gf.Path)
		if err != nil {
			t.Errorf("%s: missing committed golden — write the file then re-run: %v", gf.Path, err)
			t.Logf("--- generated %s ---\n%s", gf.Path, gf.Contents)
			continue
		}
		if string(want) != string(gf.Contents) {
			t.Errorf("%s: drifted from golden", gf.Path)
			t.Logf("--- generated ---\n%s", gf.Contents)
			t.Logf("--- golden ---\n%s", want)
		}
	}
}

// TestGenerateRejectsGenerics — Generate must hard-error when a generic
// origin or instantiation reaches it. Skipping with a warning is unsafe:
// a non-generic parent referencing the generic instantiation would still
// emit and call a non-existent MarshalGSBM method, producing a file that
// fails `go build`. This is defense-in-depth for callers that bypass
// gsbmschema.Validate (which surfaces type/generic earlier).
func TestGenerateRejectsGenerics(t *testing.T) {
	src := `package p

type Box[T any] struct {
	Value T ` + "`bin:\"1\"`" + `
}

//gsbm:root
type Root struct {
	B Box[int] ` + "`bin:\"1\"`" + `
}
`
	ps, err := gsbmschema.ParseSource("p", []string{src})
	if err != nil {
		t.Fatal(err)
	}
	roots, _ := gsbmschema.Discover(ps)
	schema, _ := gsbmschema.BuildSchema(ps, roots)
	if _, err := gsbmcodegen.Generate(ps, schema); err == nil {
		t.Fatal("expected Generate to error on generic type, got nil")
	} else if !strings.Contains(err.Error(), "generic") {
		t.Fatalf("expected error mentioning 'generic', got %v", err)
	}
	if _, err := gsbmcodegen.GenerateArena(ps, schema); err == nil {
		t.Fatal("expected GenerateArena to error on generic type, got nil")
	} else if !strings.Contains(err.Error(), "generic") {
		t.Fatalf("expected arena error mentioning 'generic', got %v", err)
	}
}

// TestGenerateAllowsOpaqueGenerics — Generate and GenerateArena must NOT
// reject a generic struct that is marked //gsbm:opaque. Opaque generics
// are skipped (handwritten Marshal/Unmarshal/Reset), and Go's
// per-instantiation generic methods make Box[int].MarshalGSBM resolve at
// the parent's call site, so the parent's generated code still compiles.
func TestGenerateAllowsOpaqueGenerics(t *testing.T) {
	src := `package p

//gsbm:opaque
type Box[T any] struct {
	Value T
}

//gsbm:root
type Root struct {
	B Box[int] ` + "`bin:\"1\"`" + `
}
`
	ps, err := gsbmschema.ParseSource("p", []string{src})
	if err != nil {
		t.Fatal(err)
	}
	roots, _ := gsbmschema.Discover(ps)
	schema, _ := gsbmschema.BuildSchema(ps, roots)
	if _, err := gsbmcodegen.Generate(ps, schema); err != nil {
		t.Fatalf("Generate must accept opaque generics, got %v", err)
	}
	if _, err := gsbmcodegen.GenerateArena(ps, schema); err != nil {
		t.Fatalf("GenerateArena must accept opaque generics, got %v", err)
	}
}

// TestGenerateRejectsIndirectOpaqueGenerics — defense-in-depth for callers
// that bypass gsbmschema.Analyze. The opaque-generic exemption only covers
// the direct-value-field case (`B Box[int]`); indirect uses (`*Box[int]`,
// `[]Box[int]`, `map[K]Box[int]`) flow through typeExpr which strips type
// arguments. Generate and GenerateArena must hard-fail before emitting
// uncompilable output.
func TestGenerateRejectsIndirectOpaqueGenerics(t *testing.T) {
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
			ps, err := gsbmschema.ParseSource("p", []string{src})
			if err != nil {
				t.Fatal(err)
			}
			roots, _ := gsbmschema.Discover(ps)
			schema, _ := gsbmschema.BuildSchema(ps, roots)
			if _, err := gsbmcodegen.Generate(ps, schema); err == nil {
				t.Fatalf("Generate must reject indirect opaque-generic %q", tc.field)
			} else if !strings.Contains(err.Error(), "generic") {
				t.Fatalf("expected error mentioning 'generic', got %v", err)
			}
			if _, err := gsbmcodegen.GenerateArena(ps, schema); err == nil {
				t.Fatalf("GenerateArena must reject indirect opaque-generic %q", tc.field)
			} else if !strings.Contains(err.Error(), "generic") {
				t.Fatalf("expected arena error mentioning 'generic', got %v", err)
			}
		})
	}
}

// TestGenerateArenaRejectsOpaqueGenericRoot — even when an opaque generic
// has handwritten methods, the arena emit body writes
// `gsbmarena.AllocStruct[Root](a)` which needs a concrete type name. A
// generic root would render as `AllocStruct[Box]` and fail to compile.
func TestGenerateArenaRejectsOpaqueGenericRoot(t *testing.T) {
	src := `package p

//gsbm:root
//gsbm:opaque
type Box[T any] struct {
	Value T
}
`
	ps, err := gsbmschema.ParseSource("p", []string{src})
	if err != nil {
		t.Fatal(err)
	}
	roots, _ := gsbmschema.Discover(ps)
	schema, _ := gsbmschema.BuildSchema(ps, roots)
	if _, err := gsbmcodegen.GenerateArena(ps, schema); err == nil {
		t.Fatal("expected GenerateArena to error on opaque generic root")
	} else if !strings.Contains(err.Error(), "generic") {
		t.Fatalf("expected error mentioning 'generic', got %v", err)
	}
}

// TestGenerateEmitsPresenceTracking asserts every generated file contains
// the bitmap-tracking lines: ClearPresence at the top of UnmarshalGSBM
// and the tail of Reset, MarkPresent on at least one known case in the
// switch, and a FieldPresent method delegating to gsbm.IsPresent. The
// fixture has at least one Marshal-only-required-primitive struct
// (Customer / Item / Total / Renamed) plus the heterogeneous Order, so
// asserting on every emitted file gives broad coverage.
func TestGenerateEmitsPresenceTracking(t *testing.T) {
	dir := fixtureDir(t)
	ps := loadFixture(t, dir)
	res := gsbmschema.Analyze(ps)
	if len(res.Issues) > 0 {
		t.Fatalf("schema issues: %s", gsbmschema.FormatIssues(res.Issues))
	}
	files, err := gsbmcodegen.Generate(ps, res.Schema)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("no files generated")
	}
	for _, gf := range files {
		body := string(gf.Contents)
		if !strings.Contains(body, "gsbm.ClearPresence(v)") {
			t.Errorf("%s: missing gsbm.ClearPresence call", gf.Path)
		}
		if !strings.Contains(body, "gsbm.MarkPresent(v,") {
			t.Errorf("%s: missing gsbm.MarkPresent call", gf.Path)
		}
		wantSig := "func (v *" + gf.TypeName + ") FieldPresent(tag uint32) bool"
		if !strings.Contains(body, wantSig) {
			t.Errorf("%s: missing FieldPresent method signature %q", gf.Path, wantSig)
		}
		if !strings.Contains(body, "return gsbm.IsPresent(v, tag)") {
			t.Errorf("%s: FieldPresent body does not delegate to gsbm.IsPresent", gf.Path)
		}
	}
}

// TestWarnIfMaxTagExceeded asserts the codegen emits a warning when a
// struct declares a tag higher than gsbm.MaxTrackedTag, and stays silent
// for tags at or below the cap. The warning is the user's only signal —
// the runtime sidecar silently no-ops on out-of-range tags — so a
// regression that drops it would be invisible without this guard.
func TestWarnIfMaxTagExceeded(t *testing.T) {
	cases := []struct {
		name    string
		tag     uint32
		warned  bool
		message string // substring expected when warned == true
	}{
		{name: "below cap", tag: 1, warned: false},
		{name: "at cap", tag: 1024, warned: false},
		{name: "above cap", tag: 1025, warned: true, message: "exceeds gsbm.MaxTrackedTag"},
		{name: "far above cap", tag: 9999, warned: true, message: "9999"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf strings.Builder
			restore := gsbmcodegen.SetWarnOut(&buf)
			t.Cleanup(func() { gsbmcodegen.SetWarnOut(restore) })

			sd := &gsbmschema.StructDecl{
				Type:   gsbmschema.TypeRef{PkgPath: "example.com/p", Name: "Big"},
				Fields: []*gsbmschema.FieldDecl{{Name: "F", Tag: tc.tag, Type: "string", Wire: gsbmschema.WireLengthDelim}},
			}
			gsbmcodegen.WarnIfMaxTagExceeded(sd)

			got := buf.String()
			if tc.warned {
				if got == "" {
					t.Fatalf("expected a warning for tag %d, got nothing", tc.tag)
				}
				if !strings.Contains(got, tc.message) {
					t.Fatalf("warning %q does not contain %q", got, tc.message)
				}
				if !strings.Contains(got, "Big") {
					t.Fatalf("warning %q does not name the offending struct", got)
				}
			} else {
				if got != "" {
					t.Fatalf("expected no warning for tag %d, got %q", tc.tag, got)
				}
			}
		})
	}
}

// TestGenerateIDRefRejectsMissingIDTag — codegen must surface a stable
// idref/missing-id-tag error when an id_ref field points at a struct
// that has no bin:"1" field. The schema validator accepts the cycle
// (the marker is present), but codegen cannot pick an ID field to
// encode, so it must hard-fail before emitting partial output.
func TestGenerateIDRefRejectsMissingIDTag(t *testing.T) {
	src := `package p

//gsbm:root
type Node struct {
	Name string ` + "`bin:\"2\"`" + `
	Next *Node  ` + "`bin:\"3,id_ref\"`" + `
}
`
	ps, err := gsbmschema.ParseSource("p", []string{src})
	if err != nil {
		t.Fatal(err)
	}
	roots, _ := gsbmschema.Discover(ps)
	schema, _ := gsbmschema.BuildSchema(ps, roots)
	if _, err := gsbmcodegen.Generate(ps, schema); err == nil {
		t.Fatal("expected Generate to error on missing bin:\"1\" id field, got nil")
	} else if !strings.Contains(err.Error(), "idref/missing-id-tag") {
		t.Fatalf("expected error code 'idref/missing-id-tag', got %v", err)
	}
}

// TestGenerateIDRefRejectsNonPrimitiveIDField — an id_ref target whose
// bin:"1" field is not a primitive (e.g., a nested struct or slice) has
// no natural leaf-scalar wire encoding. Codegen must surface
// idref/missing-id-tag rather than emit code that would call a struct
// MarshalGSBM into the leaf-scalar slot.
func TestGenerateIDRefRejectsNonPrimitiveIDField(t *testing.T) {
	src := `package p

type Key struct {
	V string ` + "`bin:\"1\"`" + `
}

//gsbm:root
type Node struct {
	ID   Key   ` + "`bin:\"1\"`" + `
	Next *Node ` + "`bin:\"2,id_ref\"`" + `
}
`
	ps, err := gsbmschema.ParseSource("p", []string{src})
	if err != nil {
		t.Fatal(err)
	}
	roots, _ := gsbmschema.Discover(ps)
	schema, _ := gsbmschema.BuildSchema(ps, roots)
	if _, err := gsbmcodegen.Generate(ps, schema); err == nil {
		t.Fatal("expected Generate to error on non-primitive bin:\"1\" id field, got nil")
	} else if !strings.Contains(err.Error(), "idref/missing-id-tag") {
		t.Fatalf("expected error code 'idref/missing-id-tag', got %v", err)
	}
}

// TestGenerateIDRefIntID — an id_ref field whose target's bin:"1" is an
// integer must produce a field key with VARINT wire type and encode the
// integer ID directly. This is the cousin of the string-ID case in the
// cyclebreak fixture; both paths share the same wireTypeForValue lookup.
func TestGenerateIDRefIntID(t *testing.T) {
	src := `package p

//gsbm:root
type Node struct {
	ID   int64 ` + "`bin:\"1\"`" + `
	Next *Node ` + "`bin:\"2,id_ref\"`" + `
}
`
	ps, err := gsbmschema.ParseSource("p", []string{src})
	if err != nil {
		t.Fatal(err)
	}
	roots, _ := gsbmschema.Discover(ps)
	schema, _ := gsbmschema.BuildSchema(ps, roots)
	files, err := gsbmcodegen.Generate(ps, schema)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("no files generated")
	}
	body := string(files[0].Contents)
	if !strings.Contains(body, "w.WriteTag(2, gsbm.WireVarint)") {
		t.Errorf("expected id_ref field key with WireVarint for int64 ID, got body:\n%s", body)
	}
	if !strings.Contains(body, "w.WriteVarint(int64(v.Next.ID))") {
		t.Errorf("expected encoder to emit Next.ID as varint, got body:\n%s", body)
	}
}

// TestGenerateSkipsExternalAndOpaque ensures the generator only produces
// files for in-set, non-opaque, non-generic structs.
func TestGenerateSkipsExternalAndOpaque(t *testing.T) {
	dir := fixtureDir(t)
	ps := loadFixture(t, dir)
	res := gsbmschema.Analyze(ps)
	files, err := gsbmcodegen.Generate(ps, res.Schema)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	for _, gf := range files {
		if !strings.HasSuffix(gf.Path, "_gsbm.go") {
			t.Errorf("unexpected output filename: %s", gf.Path)
		}
		if !strings.Contains(string(gf.Contents), "Code generated by gsbmcodegen") {
			t.Errorf("%s: missing generated header", gf.Path)
		}
	}
}
