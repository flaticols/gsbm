package odmcodegen_test

import (
	"os"
	"testing"

	"github.com/flaticols/gsbm/tools/odmcodegen"
	"github.com/flaticols/gsbm/tools/odmschema"
)

// TestRegenGoldenSample is opt-in (REGEN_GOLDEN=1) and rewrites the
// committed _odm.go files from the live fixture. Useful after a codegen
// change so the maintainer doesn't hand-paste from t.Logf output.
func TestRegenGoldenSample(t *testing.T) {
	if os.Getenv("REGEN_GOLDEN") != "1" {
		t.Skip("set REGEN_GOLDEN=1 to rewrite goldens")
	}
	dir := fixtureDir(t)
	ps := loadHandwrittenOnly(t, dir)
	res := odmschema.Analyze(ps)
	if len(res.Issues) > 0 {
		t.Fatalf("schema issues: %s", odmschema.FormatIssues(res.Issues))
	}
	files, err := odmcodegen.Generate(ps, res.Schema)
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
