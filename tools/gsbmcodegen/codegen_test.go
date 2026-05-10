package gsbmcodegen_test

import (
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
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

// loadHandwrittenOnly parses the fixture's handwritten Go files (types.go
// and friends) but skips committed *_gsbm.go siblings. The committed files
// import storage/gsbm via the module path, which gsbmschema.LoadFromDirs's
// stdlib-only importer can't resolve. The handwritten types.go has no
// imports, so this lightweight loader is sufficient for the codegen test.
func loadHandwrittenOnly(t *testing.T, dir string) *gsbmschema.PackageSet {
	t.Helper()
	fset := token.NewFileSet()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	var files []*ast.File
	var pkgName string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		if strings.HasSuffix(e.Name(), "_test.go") ||
			strings.HasSuffix(e.Name(), "_gsbm.go") ||
			strings.HasSuffix(e.Name(), "_gsbm_arena.go") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		f, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		if pkgName == "" {
			pkgName = f.Name.Name
		}
		files = append(files, f)
	}
	conf := &types.Config{Importer: importer.Default()}
	info := &types.Info{
		Types:      map[ast.Expr]types.TypeAndValue{},
		Defs:       map[*ast.Ident]types.Object{},
		Uses:       map[*ast.Ident]types.Object{},
		Implicits:  map[ast.Node]types.Object{},
		Selections: map[*ast.SelectorExpr]*types.Selection{},
		Scopes:     map[ast.Node]*types.Scope{},
		Instances:  map[*ast.Ident]types.Instance{},
	}
	pkgPath := "go.flaticols.dev/gsbm/tools/gsbmcodegen/fixtures/sample"
	pkg, err := conf.Check(pkgPath, fset, files, info)
	if err != nil {
		t.Fatalf("typecheck: %v", err)
	}
	return &gsbmschema.PackageSet{
		Fset: fset,
		Packages: []*gsbmschema.Package{{
			Path:  pkg.Path(),
			Name:  pkg.Name(),
			Files: files,
			Info:  info,
			Pkg:   pkg,
		}},
	}
}

// TestGoldenSample asserts every committed <type>_gsbm.go is byte-identical
// to what Generate produces from the live fixture. This is the contract
// that lets us hand-edit the fixture and regenerate without surprises.
func TestGoldenSample(t *testing.T) {
	dir := fixtureDir(t)
	ps := loadHandwrittenOnly(t, dir)
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
	ps := loadHandwrittenOnly(t, dir)
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
	// Order, Renamed, DeepNested, and MapWithPointer are the //gsbm:root
	// types in the fixture. DeepNested is the recursive-eviction fixture
	// (see types.go); MapWithPointer is the map-of-struct cleanup
	// fixture — keep them on the roots list so their arena helpers are
	// regenerated alongside the others.
	wantRoots := map[string]bool{"Order": true, "Renamed": true, "DeepNested": true, "MapWithPointer": true}
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

func (b *Box[T]) MarshalGSBM(w []byte) []byte             { return w }
func (b *Box[T]) UnmarshalGSBM(d []byte) ([]byte, error)  { return d, nil }
func (b *Box[T]) Reset()                                   {}
func (b *Box[T]) ForgetPresenceTree()                      {}
func (b *Box[T]) ForgetValuePresenceTree()                 {}

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

// TestGenerateRequiresOpaqueForgetMethods — Generate emits
// ForgetPresenceTree / ForgetValuePresenceTree calls on every struct-typed
// field of a generated parent. Opaque types are emit-skipped, so they must
// supply both helpers themselves; otherwise the generated parent fails to
// compile with a confusing missing-method error. The precheck must surface
// this at codegen time, in each of the four reference shapes.
func TestGenerateRequiresOpaqueForgetMethods(t *testing.T) {
	cases := []struct {
		name  string
		field string
	}{
		{"value", "B Box"},
		{"pointer", "B *Box"},
		{"slice", "B []Box"},
		{"map", "B map[string]Box"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := `package p

//gsbm:opaque
type Box struct {
	Value int
}

func (b *Box) MarshalGSBM(w []byte) []byte    { return w }
func (b *Box) UnmarshalGSBM(d []byte) ([]byte, error) { return d, nil }
func (b *Box) Reset()                          {}

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
			_, err = gsbmcodegen.Generate(ps, schema)
			if err == nil {
				t.Fatalf("Generate must reject opaque %q without Forget* helpers", tc.field)
			}
			if !strings.Contains(err.Error(), "ForgetPresenceTree") {
				t.Fatalf("expected error mentioning 'ForgetPresenceTree', got %v", err)
			}
		})
	}
}

// TestGenerateAllowsOpaqueWithForgetMethods — when the opaque type supplies
// the two Forget* helpers, Generate must accept it in every reference shape.
func TestGenerateAllowsOpaqueWithForgetMethods(t *testing.T) {
	src := `package p

//gsbm:opaque
type Box struct {
	Value int
}

func (b *Box) MarshalGSBM(w []byte) []byte             { return w }
func (b *Box) UnmarshalGSBM(d []byte) ([]byte, error)  { return d, nil }
func (b *Box) Reset()                                   {}
func (b *Box) ForgetPresenceTree()                      {}
func (b *Box) ForgetValuePresenceTree()                 {}

//gsbm:root
type Root struct {
	V Box             ` + "`bin:\"1\"`" + `
	P *Box            ` + "`bin:\"2\"`" + `
	S []Box           ` + "`bin:\"3\"`" + `
	M map[string]Box  ` + "`bin:\"4\"`" + `
}
`
	ps, err := gsbmschema.ParseSource("p", []string{src})
	if err != nil {
		t.Fatal(err)
	}
	roots, _ := gsbmschema.Discover(ps)
	schema, _ := gsbmschema.BuildSchema(ps, roots)
	if _, err := gsbmcodegen.Generate(ps, schema); err != nil {
		t.Fatalf("Generate must accept opaque with Forget* helpers, got %v", err)
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
	ps := loadHandwrittenOnly(t, dir)
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

// TestGenerateSkipsExternalAndOpaque ensures the generator only produces
// files for in-set, non-opaque, non-generic structs.
func TestGenerateSkipsExternalAndOpaque(t *testing.T) {
	dir := fixtureDir(t)
	ps := loadHandwrittenOnly(t, dir)
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
