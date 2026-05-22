package customcodec_test

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"go.flaticols.dev/gsbm/tools/gsbmcodegen"
	"go.flaticols.dev/gsbm/tools/gsbmcodegen/codecs"
	"go.flaticols.dev/gsbm/tools/gsbmcodegen/codecs/builtins"
	"go.flaticols.dev/gsbm/tools/gsbmschema"
)

// fixtureDir resolves this fixture's directory regardless of cwd.
func fixtureDir(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Dir(thisFile)
}

// fixtureRegistry returns the codec registry the customcodec fixture needs:
// the built-in Time plus a DecimalString decl bound to the local
// DecimalAmount type. The encode/decode functions live in this package
// (codec.go), so PkgImport equals the fixture's own import path and the
// emitter will render unqualified calls (`EncodeDecimalAmount`).
func fixtureRegistry(t *testing.T) *codecs.Registry {
	t.Helper()
	reg := builtins.NewBuiltinRegistry()
	if err := reg.Register(builtins.NewDecimalStringDecl(
		"DecimalString",
		"go.flaticols.dev/gsbm/tools/gsbmcodegen/fixtures/customcodec.DecimalAmount",
		"EmitDecimalAmount",
		"DecodeDecimalAmount",
		"go.flaticols.dev/gsbm/tools/gsbmcodegen/fixtures/customcodec",
	)); err != nil {
		t.Fatalf("register DecimalString: %v", err)
	}
	if err := reg.Register(builtins.NewDecimalAppendDecl(
		"DecimalAppend",
		"go.flaticols.dev/gsbm/tools/gsbmcodegen/fixtures/customcodec.DecimalAmount",
		"EmitDecimalAmountAppend",
		"DecodeDecimalAmountAppend",
		"go.flaticols.dev/gsbm/tools/gsbmcodegen/fixtures/customcodec",
	)); err != nil {
		t.Fatalf("register DecimalAppend: %v", err)
	}
	if err := reg.Register(builtins.NewDecimalBinaryDecl(
		"DecimalBinary",
		"go.flaticols.dev/gsbm/tools/gsbmcodegen/fixtures/customcodec.DecimalAmount",
		"EncodeDecimalAmountBinary",
		"DecodeDecimalAmountBinary",
		"SizeDecimalAmountBinary",
		"go.flaticols.dev/gsbm/tools/gsbmcodegen/fixtures/customcodec",
	)); err != nil {
		t.Fatalf("register DecimalBinary: %v", err)
	}
	if err := reg.Register(builtins.NewStreamingJSONDecl(
		"StreamingJSON",
		"go.flaticols.dev/gsbm/tools/gsbmcodegen/fixtures/customcodec.LargePayload",
		"StreamLargePayload",
		"DecodeLargePayload",
		"go.flaticols.dev/gsbm/tools/gsbmcodegen/fixtures/customcodec",
	)); err != nil {
		t.Fatalf("register StreamingJSON: %v", err)
	}
	return reg
}

func loadFixture(t *testing.T, dir string) *gsbmschema.PackageSet {
	t.Helper()
	ps, err := gsbmschema.LoadFromDirs([]string{dir})
	if err != nil {
		t.Fatalf("LoadFromDirs(%s): %v", dir, err)
	}
	return ps
}

// TestGoldenCustomCodec asserts the committed record_gsbm.go is
// byte-identical to what GenerateWithCodecs produces from the live
// types.go, using the fixture's custom-codec registry.
func TestGoldenCustomCodec(t *testing.T) {
	dir := fixtureDir(t)
	ps := loadFixture(t, dir)
	res := gsbmschema.Analyze(ps)
	if len(res.Issues) > 0 {
		t.Fatalf("schema issues: %s", gsbmschema.FormatIssues(res.Issues))
	}
	files, err := gsbmcodegen.GenerateWithCodecs(ps, res.Schema, fixtureRegistry(t))
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

// TestGoldenCustomCodecArena pins the arena-mode companion for Record.
// GenerateArena doesn't depend on the codec registry (it emits the
// Decode/Detach helpers that wrap the heap-mode UnmarshalGSBM body), so
// the standard signature stands.
func TestGoldenCustomCodecArena(t *testing.T) {
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

func TestRegenGolden(t *testing.T) {
	if os.Getenv("REGEN_GOLDEN") != "1" {
		t.Skip("set REGEN_GOLDEN=1 to rewrite goldens")
	}
	dir := fixtureDir(t)
	ps := loadFixture(t, dir)
	res := gsbmschema.Analyze(ps)
	if len(res.Issues) > 0 {
		t.Fatalf("schema issues: %s", gsbmschema.FormatIssues(res.Issues))
	}
	files, err := gsbmcodegen.GenerateWithCodecs(ps, res.Schema, fixtureRegistry(t))
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

func TestRegenGoldenArena(t *testing.T) {
	if os.Getenv("REGEN_GOLDEN") != "1" {
		t.Skip("set REGEN_GOLDEN=1 to rewrite goldens")
	}
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
	for _, gf := range files {
		if err := os.WriteFile(gf.Path, gf.Contents, 0o644); err != nil {
			t.Fatalf("write %s: %v", gf.Path, err)
		}
		t.Logf("wrote %s", gf.Path)
	}
}
