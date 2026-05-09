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
