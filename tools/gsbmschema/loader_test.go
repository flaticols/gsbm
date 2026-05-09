package odmschema

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLoadFromDirsKeepsDistinctPackagesSeparate — two leaf dirs that
// happen to share a package clause name (`package model`) must NOT
// collapse into one PkgPath. Otherwise the schema dedup key
// (`PkgPath + "." + Name`) merges distinct types and the closure walk
// drops or aliases roots silently.
func TestLoadFromDirsKeepsDistinctPackagesSeparate(t *testing.T) {
	root := t.TempDir()
	dirA := filepath.Join(root, "a")
	dirB := filepath.Join(root, "b")
	for _, d := range []string{dirA, dirB} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	srcA := []byte(`package model

//odm:root
type Order struct {
	ID uint64 ` + "`bin:\"1\"`" + `
}
`)
	srcB := []byte(`package model

//odm:root
type Order struct {
	ID uint64 ` + "`bin:\"1\"`" + `
	Sku string ` + "`bin:\"2\"`" + `
}
`)
	if err := os.WriteFile(filepath.Join(dirA, "model.go"), srcA, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dirB, "model.go"), srcB, 0o644); err != nil {
		t.Fatal(err)
	}
	ps, err := LoadFromDirs([]string{dirA, dirB})
	if err != nil {
		t.Fatalf("LoadFromDirs: %v", err)
	}
	if len(ps.Packages) != 2 {
		t.Fatalf("expected 2 packages, got %d", len(ps.Packages))
	}
	if ps.Packages[0].Path == ps.Packages[1].Path {
		t.Fatalf("packages share PkgPath %q — distinct dirs must produce distinct paths",
			ps.Packages[0].Path)
	}
	roots, _ := Discover(ps)
	if len(roots) != 2 {
		t.Fatalf("expected both roots discovered, got %d", len(roots))
	}
	schema, issues := BuildSchema(ps, roots)
	if len(issues) != 0 {
		t.Fatalf("unexpected issues: %v", issues)
	}
	// Both Order structs must survive as distinct entries — different
	// PkgPath, same Name. With the bug they would collapse to one.
	var orders int
	for _, sd := range schema.Structs {
		if sd.Type.Name == "Order" {
			orders++
		}
	}
	if orders != 2 {
		t.Fatalf("expected 2 distinct Order structs in closure, got %d", orders)
	}
}

// TestLoadFromDirsStableAcrossInvocationStyles — the same Go package
// referenced via a relative directory and via its absolute path must
// produce the same PkgPath. Otherwise schVer, schema.yaml, and the
// snapshot diff classifier see "different packages" and the hash
// becomes unstable across CI / local / different working directories.
func TestLoadFromDirsStableAcrossInvocationStyles(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"),
		[]byte("module example.com/proj\n\ngo 1.26\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "pkg", "model")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	src := []byte(`package model

//odm:root
type Order struct {
	ID uint64 ` + "`bin:\"1\"`" + `
}
`)
	if err := os.WriteFile(filepath.Join(dir, "model.go"), src, 0o644); err != nil {
		t.Fatal(err)
	}

	// Absolute invocation.
	psAbs, err := LoadFromDirs([]string{dir})
	if err != nil {
		t.Fatalf("LoadFromDirs(abs): %v", err)
	}

	// Relative invocation: cd into the module root and pass `pkg/model`.
	prev, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(root); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(prev) })
	psRel, err := LoadFromDirs([]string{filepath.Join("pkg", "model")})
	if err != nil {
		t.Fatalf("LoadFromDirs(rel): %v", err)
	}

	const want = "example.com/proj/pkg/model"
	if psAbs.Packages[0].Path != want {
		t.Fatalf("absolute invocation: got PkgPath %q, want %q", psAbs.Packages[0].Path, want)
	}
	if psRel.Packages[0].Path != want {
		t.Fatalf("relative invocation: got PkgPath %q, want %q", psRel.Packages[0].Path, want)
	}
}

// TestLoadFromDirsCrossPackageMarkers — when one input dir imports another
// input dir, markers on the imported package's structs (//odm:opaque,
// //odm:reserved, //odm:allow-breaking, field //odm:cycle_break_via_id)
// MUST flow through to the schema. The naive design (typecheck each dir
// in isolation through importer.Default()) silently dropped them: the
// imported package's *types.Package was a different pointer than our
// parsed one, so the AST never attached. Validation passed because the
// imported package's PkgPath was in the allowed input set, hiding the
// regression. This test fails on that design and passes once the loader
// resolves cross-input imports against its own parsed packages.
func TestLoadFromDirsCrossPackageMarkers(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"),
		[]byte("module example.com/proj\n\ngo 1.26\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	leafDir := filepath.Join(root, "leaf")
	rootDir := filepath.Join(root, "root")
	for _, d := range []string{leafDir, rootDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// leaf has an opaque struct; if markers don't flow, schema reports
	// Opaque=false and the closure walker would descend into Inner's
	// fields (here, an unsupported chan field which would be flagged).
	leafSrc := []byte(`package leaf

//odm:opaque
type Inner struct {
	C chan int
}
`)
	rootSrc := []byte(`package root

import "example.com/proj/leaf"

//odm:root
type Outer struct {
	ID    uint64     ` + "`bin:\"1\"`" + `
	Inner leaf.Inner ` + "`bin:\"2\"`" + `
}
`)
	if err := os.WriteFile(filepath.Join(leafDir, "leaf.go"), leafSrc, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootDir, "root.go"), rootSrc, 0o644); err != nil {
		t.Fatal(err)
	}
	ps, err := LoadFromDirs([]string{rootDir, leafDir})
	if err != nil {
		t.Fatalf("LoadFromDirs: %v", err)
	}
	roots, _ := Discover(ps)
	if len(roots) != 1 {
		t.Fatalf("expected 1 root, got %d", len(roots))
	}
	schema, issues := BuildSchema(ps, roots)
	for _, iss := range issues {
		// chan inside Inner must NOT surface as type/unsupported — Inner is
		// opaque, the closure must terminate at Inner without inspecting C.
		if iss.Code == "type/unsupported" {
			t.Errorf("unexpected unsupported-type issue (Inner should be opaque): %s", iss.Message)
		}
	}
	var innerSD *StructDecl
	for _, sd := range schema.Structs {
		if sd.Type.Name == "Inner" && sd.Type.PkgPath == "example.com/proj/leaf" {
			innerSD = sd
			break
		}
	}
	if innerSD == nil {
		t.Fatalf("leaf.Inner not present in schema closure")
	}
	if !innerSD.Opaque {
		t.Fatalf("leaf.Inner.Opaque = false, want true (//odm:opaque on imported package was dropped)")
	}
}

// TestLoadFromDirsRejectsDuplicateInput — passing the same dir twice is
// a user error; the loader must surface it instead of silently merging
// or producing a duplicate-package typecheck error.
func TestLoadFromDirsRejectsDuplicateInput(t *testing.T) {
	dir := t.TempDir()
	src := []byte(`package model

//odm:root
type Order struct {
	ID uint64 ` + "`bin:\"1\"`" + `
}
`)
	if err := os.WriteFile(filepath.Join(dir, "model.go"), src, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := LoadFromDirs([]string{dir, dir})
	if err == nil {
		t.Fatal("expected error for duplicate dir input, got nil")
	}
}
