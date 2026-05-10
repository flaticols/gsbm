package evolution

import (
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.flaticols.dev/gsbm/tools/gsbmcodegen"
	"go.flaticols.dev/gsbm/tools/gsbmschema"
)

// goldenScenarios are the schema-valid pair members that get committed
// goldens. The breaking scenarios (wirechange, typechange, tagchange,
// removefield) only need their source to be parseable — no codegen runs
// on them, so they have no goldens. Task 4 round-trip tests consume the
// addfield goldens from both sides; compatwrite goldens are exercised by
// Task 4's lifecycle assertions.
var goldenScenarios = []struct {
	scenario string
	side     string
}{
	{"addfield", "before"},
	{"addfield", "after"},
	{"compatwrite", "before"},
	{"compatwrite", "after"},
}

func goldenScenarioDir(t *testing.T, scenario, side string) string {
	t.Helper()
	return filepath.Join(fixtureRoot(t), scenario, side)
}

// loadHandwrittenOnly mirrors the helper in fixtures/graph: parses the
// fixture's handwritten Go files (types.go) but skips committed *_gsbm.go
// siblings and *_test.go files. The committed files import storage/gsbm
// via the module path which the stdlib-only importer cannot resolve.
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
	rel, err := filepath.Rel(fixtureRoot(t), dir)
	if err != nil {
		t.Fatalf("rel: %v", err)
	}
	pkgPath := "go.flaticols.dev/gsbm/tools/gsbmcodegen/fixtures/evolution/" + filepath.ToSlash(rel)
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

// TestGoldenEvolution asserts every committed *_gsbm.go file under the
// schema-valid evolution scenarios is byte-identical to what Generate
// produces from the live types.go. Drift signals either an unintended
// codegen change or a stale fixture; rerun with REGEN_GOLDEN=1 to
// rewrite if the new output is intentional.
func TestGoldenEvolution(t *testing.T) {
	for _, sc := range goldenScenarios {
		dir := goldenScenarioDir(t, sc.scenario, sc.side)
		t.Run(sc.scenario+"/"+sc.side, func(t *testing.T) {
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
		})
	}
}

// TestGoldenEvolutionArena is the arena-mode counterpart. Each scenario
// has Shipment as a //gsbm:root, so GenerateArena emits one file per
// side.
func TestGoldenEvolutionArena(t *testing.T) {
	for _, sc := range goldenScenarios {
		dir := goldenScenarioDir(t, sc.scenario, sc.side)
		t.Run(sc.scenario+"/"+sc.side, func(t *testing.T) {
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
		})
	}
}

// TestRegenGolden is opt-in (REGEN_GOLDEN=1) and rewrites the committed
// *_gsbm.go files for every schema-valid scenario.
func TestRegenGolden(t *testing.T) {
	if os.Getenv("REGEN_GOLDEN") != "1" {
		t.Skip("set REGEN_GOLDEN=1 to rewrite goldens")
	}
	for _, sc := range goldenScenarios {
		dir := goldenScenarioDir(t, sc.scenario, sc.side)
		ps := loadHandwrittenOnly(t, dir)
		res := gsbmschema.Analyze(ps)
		if len(res.Issues) > 0 {
			t.Fatalf("%s/%s: schema issues: %s", sc.scenario, sc.side, gsbmschema.FormatIssues(res.Issues))
		}
		files, err := gsbmcodegen.Generate(ps, res.Schema)
		if err != nil {
			t.Fatalf("%s/%s: generate: %v", sc.scenario, sc.side, err)
		}
		for _, gf := range files {
			if err := os.WriteFile(gf.Path, gf.Contents, 0o644); err != nil {
				t.Fatalf("write %s: %v", gf.Path, err)
			}
			t.Logf("wrote %s", gf.Path)
		}
	}
}

// TestRegenGoldenArena rewrites the arena-mode goldens for every
// schema-valid scenario.
func TestRegenGoldenArena(t *testing.T) {
	if os.Getenv("REGEN_GOLDEN") != "1" {
		t.Skip("set REGEN_GOLDEN=1 to rewrite goldens")
	}
	for _, sc := range goldenScenarios {
		dir := goldenScenarioDir(t, sc.scenario, sc.side)
		ps := loadHandwrittenOnly(t, dir)
		res := gsbmschema.Analyze(ps)
		if len(res.Issues) > 0 {
			t.Fatalf("%s/%s: schema issues: %s", sc.scenario, sc.side, gsbmschema.FormatIssues(res.Issues))
		}
		files, err := gsbmcodegen.GenerateArena(ps, res.Schema)
		if err != nil {
			t.Fatalf("%s/%s: generate arena: %v", sc.scenario, sc.side, err)
		}
		for _, gf := range files {
			if err := os.WriteFile(gf.Path, gf.Contents, 0o644); err != nil {
				t.Fatalf("write %s: %v", gf.Path, err)
			}
			t.Logf("wrote %s", gf.Path)
		}
	}
}
