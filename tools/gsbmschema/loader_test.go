package gsbmschema

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestLoadFromDirsKeepsDistinctPackagesSeparate — two leaf dirs that
// happen to share a package clause name (`package model`) must NOT
// collapse into one PkgPath. Otherwise the schema dedup key
// (`PkgPath + "." + Name`) merges distinct types and the closure walk
// drops or aliases roots silently.
func TestLoadFromDirsKeepsDistinctPackagesSeparate(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"),
		[]byte("module example.com/proj\n\ngo 1.26\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dirA := filepath.Join(root, "a")
	dirB := filepath.Join(root, "b")
	for _, d := range []string{dirA, dirB} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	srcA := []byte(`package model

//gsbm:root
type Order struct {
	ID uint64 ` + "`bin:\"1\"`" + `
}
`)
	srcB := []byte(`package model

//gsbm:root
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
// produce the same PkgPath. Otherwise schemaHint, schema.yaml, and the
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

//gsbm:root
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
	t.Chdir(root)
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
// input dir, markers on the imported package's structs (//gsbm:opaque,
// //gsbm:reserved, //gsbm:allow-breaking, field //gsbm:cycle_break_via_id)
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

//gsbm:opaque
type Inner struct {
	C chan int
}
`)
	rootSrc := []byte(`package root

import "example.com/proj/leaf"

//gsbm:root
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
		t.Fatalf("leaf.Inner.Opaque = false, want true (//gsbm:opaque on imported package was dropped)")
	}
}

// TestLoadFromDirsRejectsDuplicateInput — passing the same dir twice is
// a user error; the loader must surface it instead of silently merging
// or producing a duplicate-package typecheck error.
func TestLoadFromDirsRejectsDuplicateInput(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"),
		[]byte("module example.com/proj\n\ngo 1.26\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "model")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	src := []byte(`package model

//gsbm:root
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

// TestLoadFromDirsSinglePackage — a single dir with one package and a
// stdlib-only dep loads cleanly, populates *PackageSet with one entry,
// and roundtrips through Discover/BuildSchema. The smallest happy-path
// regression for the go/packages-backed loader.
func TestLoadFromDirsSinglePackage(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"),
		[]byte("module example.com/single\n\ngo 1.26\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "model")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	src := []byte(`package model

import "time"

//gsbm:root
type Order struct {
	ID      uint64    ` + "`bin:\"1\"`" + `
	Created time.Time ` + "`bin:\"2\" gsbm:\"opaque\"`" + `
}
`)
	if err := os.WriteFile(filepath.Join(dir, "model.go"), src, 0o644); err != nil {
		t.Fatal(err)
	}
	ps, err := LoadFromDirs([]string{dir})
	if err != nil {
		t.Fatalf("LoadFromDirs: %v", err)
	}
	if len(ps.Packages) != 1 {
		t.Fatalf("expected 1 package, got %d", len(ps.Packages))
	}
	if got, want := ps.Packages[0].Path, "example.com/single/model"; got != want {
		t.Fatalf("PkgPath = %q, want %q", got, want)
	}
	if ps.Packages[0].Pkg == nil {
		t.Fatal("Pkg is nil — typecheck output not propagated")
	}
	if ps.Packages[0].Info == nil {
		t.Fatal("Info is nil — types.Info not propagated")
	}
	if len(ps.Packages[0].Files) != 1 {
		t.Fatalf("expected 1 source file, got %d", len(ps.Packages[0].Files))
	}
	roots, _ := Discover(ps)
	if len(roots) != 1 {
		t.Fatalf("expected 1 root, got %d", len(roots))
	}
}

// TestLoadFromDirsResolvesInternalTransitiveDeps — a single input dir
// that imports a sibling package which itself imports a third package
// (deep transitive chain) loads without error. This is the case the old
// importer.Default()-backed loader could not handle: it could parse the
// input dir but failed to typecheck because its sibling and grand-
// sibling packages were not on the supplied dir list, and
// importer.Default() can only return export data for installed
// packages (which test temp dirs are not).
//
// With the go/packages-backed loader, NeedDeps walks the module's
// internal graph automatically, so a root package's imports get
// resolved even when only the root dir is supplied.
func TestLoadFromDirsResolvesInternalTransitiveDeps(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"),
		[]byte("module example.com/transitive\n\ngo 1.26\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rootDir := filepath.Join(root, "rootpkg")
	midDir := filepath.Join(root, "internal", "mid")
	leafDir := filepath.Join(root, "internal", "leaf")
	for _, d := range []string{rootDir, midDir, leafDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	leafSrc := []byte(`package leaf

type ID uint64
`)
	midSrc := []byte(`package mid

import "example.com/transitive/internal/leaf"

type Wrapper struct {
	V leaf.ID
}
`)
	rootSrc := []byte(`package rootpkg

import "example.com/transitive/internal/mid"

//gsbm:root
type Order struct {
	ID    uint64       ` + "`bin:\"1\"`" + `
	Inner mid.Wrapper  ` + "`bin:\"2\"`" + `
}
`)
	if err := os.WriteFile(filepath.Join(leafDir, "leaf.go"), leafSrc, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(midDir, "mid.go"), midSrc, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootDir, "root.go"), rootSrc, 0o644); err != nil {
		t.Fatal(err)
	}
	// Note: only rootDir is passed. mid + leaf must be resolved
	// transitively by the loader. The old loader would fail with
	// `could not import example.com/transitive/internal/mid`.
	ps, err := LoadFromDirs([]string{rootDir})
	if err != nil {
		t.Fatalf("LoadFromDirs(rootDir only): %v — internal transitive deps must resolve via the module graph", err)
	}
	var rootPkg *Package
	for _, p := range ps.Packages {
		if p.Path == "example.com/transitive/rootpkg" {
			rootPkg = p
			break
		}
	}
	if rootPkg == nil {
		t.Fatalf("rootpkg not in package set (got %d pkgs)", len(ps.Packages))
	}
	if rootPkg.Pkg == nil {
		t.Fatal("rootpkg typecheck output missing")
	}
}

// TestParseSourceShapeMatchesLoader — the hermetic ParseSource helper
// produces a *PackageSet shape (Path, Name, Files, Info, Pkg populated)
// that is observably the same as what LoadFromDirs produces, modulo
// PkgPath. This pins the contract that test fixtures using ParseSource
// see the same Package surface as production code paths.
func TestParseSourceShapeMatchesLoader(t *testing.T) {
	src := `package shape

//gsbm:root
type Item struct {
	ID uint64 ` + "`bin:\"1\"`" + `
}
`
	ps, err := ParseSource("shape", []string{src})
	if err != nil {
		t.Fatalf("ParseSource: %v", err)
	}
	if len(ps.Packages) != 1 {
		t.Fatalf("expected 1 package, got %d", len(ps.Packages))
	}
	p := ps.Packages[0]
	if p.Name != "shape" {
		t.Fatalf("Name = %q, want %q", p.Name, "shape")
	}
	if p.Path == "" {
		t.Fatal("Path empty")
	}
	if p.Pkg == nil || p.Info == nil {
		t.Fatal("Pkg/Info not populated")
	}
	if len(p.Files) != 1 {
		t.Fatalf("expected 1 file, got %d", len(p.Files))
	}
	roots, _ := Discover(ps)
	if len(roots) != 1 {
		t.Fatalf("expected 1 root, got %d", len(roots))
	}
}

// modulefixturePath returns the absolute path to the bundled
// testdata/modulefixture Go module. It is a fully-formed module with
// its own go.mod; tests t.Chdir into it before invoking
// LoadFromPatterns so packages.Load anchors module resolution there.
func modulefixturePath(t *testing.T) string {
	t.Helper()
	abs, err := filepath.Abs(filepath.Join("testdata", "modulefixture"))
	if err != nil {
		t.Fatalf("abs testdata/modulefixture: %v", err)
	}
	return abs
}

// TestLoadFromPatternsLoadsModuleFixture — `LoadFromPatterns(["./..."])`
// run from a module's root resolves every package under the module via
// the wildcard. Confirms the new pattern-based entry point produces a
// PackageSet with both fixture packages (api + internal/inner) populated
// with type info and AST files.
func TestLoadFromPatternsLoadsModuleFixture(t *testing.T) {
	dir := modulefixturePath(t)
	t.Chdir(dir)

	ps, err := LoadFromPatterns([]string{"./..."})
	if err != nil {
		t.Fatalf("LoadFromPatterns: %v", err)
	}
	paths := make([]string, 0, len(ps.Packages))
	for _, p := range ps.Packages {
		paths = append(paths, p.Path)
		if p.Pkg == nil {
			t.Errorf("%s: Pkg nil", p.Path)
		}
		if p.Info == nil {
			t.Errorf("%s: Info nil", p.Path)
		}
		if len(p.Files) == 0 {
			t.Errorf("%s: no Files", p.Path)
		}
	}
	sort.Strings(paths)
	want := []string{
		"example.com/modulefixture/api",
		"example.com/modulefixture/internal/inner",
	}
	if len(paths) != len(want) {
		t.Fatalf("got packages %v, want %v", paths, want)
	}
	for i, w := range want {
		if paths[i] != w {
			t.Fatalf("packages[%d] = %q, want %q (full list: %v)", i, paths[i], w, paths)
		}
	}
}

// TestLoadFromPatternsResolvesInternalImport — the central case from
// the issue: a single root package (api) imports a sibling internal/
// package, and the loader typechecks the root without the caller
// enumerating the internal/ dir on the command line. The old
// importer.Default()-backed loader produced
// `could not import example.com/modulefixture/internal/inner` here.
//
// Pattern selects only ./api; transitive deps need not appear in the
// returned set, but they MUST resolve far enough that the api package
// typechecks cleanly. (Cross-package marker flow is exercised by
// TestLoadFromDirsCrossPackageMarkers and TestLoadFromPatternsWildcardCrossPackageMarkers.)
func TestLoadFromPatternsResolvesInternalImport(t *testing.T) {
	dir := modulefixturePath(t)
	t.Chdir(dir)

	ps, err := LoadFromPatterns([]string{"./api"})
	if err != nil {
		t.Fatalf("LoadFromPatterns(./api): %v — internal import must resolve via the module graph", err)
	}
	var apiPkg *Package
	for _, p := range ps.Packages {
		if p.Path == "example.com/modulefixture/api" {
			apiPkg = p
			break
		}
	}
	if apiPkg == nil {
		t.Fatalf("api package not in set (got %d pkgs)", len(ps.Packages))
	}
	if apiPkg.Pkg == nil {
		t.Fatal("api typecheck output missing — internal/inner import failed to resolve")
	}
}

// TestLoadFromPatternsWildcardCrossPackageMarkers — when a wildcard
// pattern (`./...`) pulls every module package into the load set,
// markers on a sibling package (//gsbm:opaque on internal/inner.Tag)
// MUST flow through to the schema, the same way they do for
// LoadFromDirs. This is the pattern-side analogue of
// TestLoadFromDirsCrossPackageMarkers.
func TestLoadFromPatternsWildcardCrossPackageMarkers(t *testing.T) {
	dir := modulefixturePath(t)
	t.Chdir(dir)

	ps, err := LoadFromPatterns([]string{"./..."})
	if err != nil {
		t.Fatalf("LoadFromPatterns(./...): %v", err)
	}
	roots, _ := Discover(ps)
	if len(roots) != 1 {
		t.Fatalf("expected 1 root, got %d", len(roots))
	}
	schema, issues := BuildSchema(ps, roots)
	for _, iss := range issues {
		if iss.Code == "type/unsupported" {
			t.Errorf("unexpected unsupported-type issue (Tag should be opaque): %s", iss.Message)
		}
	}
	var tagSD *StructDecl
	for _, sd := range schema.Structs {
		if sd.Type.Name == "Tag" && sd.Type.PkgPath == "example.com/modulefixture/internal/inner" {
			tagSD = sd
			break
		}
	}
	if tagSD == nil {
		t.Fatalf("internal/inner.Tag not present in schema closure (got %d structs)", len(schema.Structs))
	}
	if !tagSD.Opaque {
		t.Fatal("internal/inner.Tag.Opaque = false; cross-package //gsbm:opaque marker dropped")
	}
}

// TestLoadFromPatternsResolvesThirdPartyDep — a synthetic module that
// requires a real third-party module (golang.org/x/sync, already in
// the gsbm repo's GOMODCACHE because it's a transitive dep of
// golang.org/x/tools) loads cleanly through LoadFromPatterns. This
// pins the headline outcome of the loader migration: third-party
// module deps resolve without any manual dir-padding.
//
// The fixture is built fresh in t.TempDir per test (cheap; ~3 small
// files). Module version + go.sum entries are read from the gsbm
// repo's own go.sum at test time so the test stays in sync when the
// upstream pin moves, and -mod=readonly verification passes without
// a network round-trip.
func TestLoadFromPatternsResolvesThirdPartyDep(t *testing.T) {
	version, sumLines := readGoSumEntries(t, "golang.org/x/sync")
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"),
		[]byte("module example.com/extdep\n\ngo 1.26\n\nrequire golang.org/x/sync "+version+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "go.sum"),
		[]byte(sumLines), 0o644); err != nil {
		t.Fatal(err)
	}
	pkgDir := filepath.Join(root, "user")
	if err := os.MkdirAll(pkgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	src := []byte(`package user

import "golang.org/x/sync/errgroup"

// Box wraps a third-party errgroup.Group so the loader is forced to
// resolve a real module dep (not just typecheck the import statement).
//gsbm:opaque
type Box struct {
	G *errgroup.Group
}
`)
	if err := os.WriteFile(filepath.Join(pkgDir, "user.go"), src, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)

	ps, err := LoadFromPatterns([]string{"./..."})
	if err != nil {
		t.Fatalf("LoadFromPatterns(./...): %v — third-party module dep must resolve", err)
	}
	var userPkg *Package
	for _, p := range ps.Packages {
		if p.Path == "example.com/extdep/user" {
			userPkg = p
			break
		}
	}
	if userPkg == nil {
		t.Fatalf("user package not in set (got %d pkgs)", len(ps.Packages))
	}
	if userPkg.Pkg == nil {
		t.Fatal("user package typecheck output missing — third-party dep failed to typecheck")
	}
}

// TestLoadFromPatternsZeroPackages — a syntactically valid pattern
// that matches no packages must surface a clear error rather than
// returning an empty PackageSet that downstream Discover/BuildSchema
// would silently treat as "no roots, nothing to do".
func TestLoadFromPatternsZeroPackages(t *testing.T) {
	dir := modulefixturePath(t)
	t.Chdir(dir)

	_, err := LoadFromPatterns([]string{"./does-not-exist/..."})
	if err == nil {
		t.Fatal("expected error for pattern matching zero packages, got nil")
	}
}

// TestLoadFromPatternsTypeError — a package that fails to typecheck
// (here: unresolved identifier) must surface the error with file:line
// position information so users can locate the bad source.
func TestLoadFromPatternsTypeError(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"),
		[]byte("module example.com/typeerr\n\ngo 1.26\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	pkgDir := filepath.Join(root, "bad")
	if err := os.MkdirAll(pkgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pkgDir, "bad.go"),
		[]byte("package bad\n\ntype Order struct {\n\tID Missing\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)

	_, err := LoadFromPatterns([]string{"./bad"})
	if err == nil {
		t.Fatal("expected error for package with type error, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "bad.go") {
		t.Fatalf("error %q should reference bad.go for actionable diagnostics", msg)
	}
	if !strings.Contains(msg, "Missing") {
		t.Fatalf("error %q should reference the unresolved identifier", msg)
	}
}

// TestLoadFromPatternsMissingModule — running from a directory with no
// go.mod ancestor must surface an error rather than silently producing
// an empty result. Module-aware loading requires a module anchor;
// callers that try this on a loose `.go` collection deserve a clear
// failure pointing at the missing go.mod.
func TestLoadFromPatternsMissingModule(t *testing.T) {
	root := t.TempDir()
	pkgDir := filepath.Join(root, "loose")
	if err := os.MkdirAll(pkgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pkgDir, "loose.go"),
		[]byte("package loose\n\ntype Order struct{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)

	_, err := LoadFromPatterns([]string{"./loose"})
	if err == nil {
		t.Fatal("expected error when running outside any Go module, got nil")
	}
	if !strings.Contains(err.Error(), "go.mod") {
		t.Fatalf("error %q should mention go.mod to guide the user", err.Error())
	}
}

// TestLoadFromPatternsRejectsEmptyInput — defensive contract: callers
// must supply at least one pattern, and individual patterns may not be
// the empty string.
func TestLoadFromPatternsRejectsEmptyInput(t *testing.T) {
	if _, err := LoadFromPatterns(nil); err == nil {
		t.Fatal("expected error for nil patterns, got nil")
	}
	if _, err := LoadFromPatterns([]string{}); err == nil {
		t.Fatal("expected error for empty patterns slice, got nil")
	}
	if _, err := LoadFromPatterns([]string{""}); err == nil {
		t.Fatal("expected error for empty pattern string, got nil")
	}
	if _, err := LoadFromPatterns([]string{"./...", ""}); err == nil {
		t.Fatal("expected error when any pattern is the empty string, got nil")
	}
}

// readGoSumEntries returns the version and the two go.sum lines (h1
// hash + go.mod hash) for module from the gsbm repo's own go.sum.
// Tests that synthesize a fixture requiring this module use the
// returned bytes so the fixture stays verifiable in -mod=readonly mode
// even after upstream version bumps.
func readGoSumEntries(t *testing.T, module string) (version, sumLines string) {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		p := filepath.Join(dir, "go.sum")
		if data, err := os.ReadFile(p); err == nil {
			prefix := module + " "
			var picked []string
			for line := range strings.SplitSeq(string(data), "\n") {
				if !strings.HasPrefix(line, prefix) {
					continue
				}
				picked = append(picked, line)
				if version == "" {
					rest := strings.TrimPrefix(line, prefix)
					if i := strings.IndexByte(rest, ' '); i > 0 {
						version = strings.TrimSuffix(rest[:i], "/go.mod")
					}
				}
			}
			if version == "" {
				t.Fatalf("module %q not found in %s", module, p)
			}
			return version, strings.Join(picked, "\n") + "\n"
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no go.sum ancestor of working dir")
		}
		dir = parent
	}
}
