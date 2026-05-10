package evolution

import (
	"os"
	"path/filepath"
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

// TestGoldenEvolution asserts every committed *_gsbm.go file under the
// schema-valid evolution scenarios is byte-identical to what Generate
// produces from the live types.go. Drift signals either an unintended
// codegen change or a stale fixture; rerun with REGEN_GOLDEN=1 to
// rewrite if the new output is intentional.
func TestGoldenEvolution(t *testing.T) {
	for _, sc := range goldenScenarios {
		dir := goldenScenarioDir(t, sc.scenario, sc.side)
		t.Run(sc.scenario+"/"+sc.side, func(t *testing.T) {
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
		ps := loadFixture(t, dir)
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
		ps := loadFixture(t, dir)
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
