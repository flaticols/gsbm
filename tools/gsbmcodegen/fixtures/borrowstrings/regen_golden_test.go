package borrowstrings_test

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"go.flaticols.dev/gsbm/tools/gsbmcodegen"
	"go.flaticols.dev/gsbm/tools/gsbmschema"
)

func fixtureDir(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Dir(thisFile)
}

func loadFixture(t *testing.T, dir string) *gsbmschema.PackageSet {
	t.Helper()
	ps, err := gsbmschema.LoadFromDirs([]string{dir})
	if err != nil {
		t.Fatalf("LoadFromDirs(%s): %v", dir, err)
	}
	return ps
}

func TestGoldenBorrowStrings(t *testing.T) {
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
	if len(files) != 2 {
		t.Fatalf("got %d generated files, want 2", len(files))
	}
	for _, gf := range files {
		want, err := os.ReadFile(gf.Path)
		if err != nil {
			t.Errorf("%s: missing committed golden: %v", gf.Path, err)
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

func TestRegenGoldenBorrowStrings(t *testing.T) {
	if os.Getenv("REGEN_GOLDEN") != "1" {
		t.Skip("set REGEN_GOLDEN=1 to rewrite goldens")
	}
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
	for _, gf := range files {
		if err := os.WriteFile(gf.Path, gf.Contents, 0o644); err != nil {
			t.Fatalf("write %s: %v", gf.Path, err)
		}
		t.Logf("wrote %s", gf.Path)
	}
}
