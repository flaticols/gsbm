package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestClassifyArg pins the dir-vs-pattern decision for the inputs
// gsbmschema's CLI dispatch hangs off. Existing-on-disk dirs are dirs;
// everything else (including bare names that look like import paths,
// and ./... wildcards that never match) is a pattern. The contract is
// "let go/packages produce the diagnostic for unresolved patterns",
// not "pre-validate".
func TestClassifyArg(t *testing.T) {
	tmp := t.TempDir()
	existingDir := filepath.Join(tmp, "real")
	if err := os.MkdirAll(existingDir, 0o755); err != nil {
		t.Fatal(err)
	}
	existingFile := filepath.Join(tmp, "file.go")
	if err := os.WriteFile(existingFile, []byte("package x\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		arg  string
		want argKind
	}{
		{"existing dir, abs path", existingDir, argDir},
		{"existing file is not a dir", existingFile, argPattern},
		{"non-existent dir-shaped", filepath.Join(tmp, "missing"), argPattern},
		{"./... wildcard", "./...", argPattern},
		{"./pkg/... wildcard", "./pkg/...", argPattern},
		{"absolute import-path wildcard", "example.com/pkg/...", argPattern},
		{"absolute import path", "example.com/pkg/foo", argPattern},
		{"bare pkg without ./", "pkg", argPattern},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyArg(tc.arg)
			if got != tc.want {
				t.Fatalf("classifyArg(%q) = %v, want %v", tc.arg, got, tc.want)
			}
		})
	}
}

// TestClassifyArgsSplit verifies the dir/pattern split preserves order
// within each list — the loader contracts treat input order as
// significant for diagnostic stability.
func TestClassifyArgsSplit(t *testing.T) {
	tmp := t.TempDir()
	dirA := filepath.Join(tmp, "a")
	dirB := filepath.Join(tmp, "b")
	for _, d := range []string{dirA, dirB} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	dirs, patterns := classifyArgs([]string{dirA, "./...", dirB, "example.com/x"})
	if len(dirs) != 2 || dirs[0] != dirA || dirs[1] != dirB {
		t.Fatalf("dirs = %v, want [%s %s]", dirs, dirA, dirB)
	}
	if len(patterns) != 2 || patterns[0] != "./..." || patterns[1] != "example.com/x" {
		t.Fatalf("patterns = %v, want [./... example.com/x]", patterns)
	}
}

// TestSmokeLintDirVsPattern is the CLI wide-net test: the sample
// fixture must lint clean when invoked with both today's dir argument
// and the new ./...-pattern argument, and the snapshots emitted by
// each form must be byte-identical. This is the "same input → same
// output regardless of argument shape" guarantee the migration is
// supposed to deliver.
func TestSmokeLintDirVsPattern(t *testing.T) {
	if testing.Short() {
		t.Skip("smoke test builds the binary; skip in -short")
	}
	repoRoot := repoRoot(t)
	bin := buildGsbmschema(t, repoRoot)

	fixtureDir := filepath.Join(repoRoot, "tools", "gsbmcodegen", "fixtures", "sample")

	t.Run("lint by dir", func(t *testing.T) {
		runOK(t, bin, repoRoot, "lint", fixtureDir)
	})
	t.Run("lint by relative dir", func(t *testing.T) {
		runOK(t, bin, repoRoot, "lint", "./tools/gsbmcodegen/fixtures/sample")
	})
	t.Run("lint by pattern", func(t *testing.T) {
		runOK(t, bin, repoRoot, "lint", "./tools/gsbmcodegen/fixtures/sample/...")
	})

	// Snapshots produced by the dir-form and pattern-form must agree
	// byte-for-byte: the loader has to converge regardless of the
	// argument shape that fed it.
	outDir := filepath.Join(t.TempDir(), "snap-dir")
	runOK(t, bin, repoRoot, "snapshot", "-o", outDir, fixtureDir)
	outPat := filepath.Join(t.TempDir(), "snap-pat")
	runOK(t, bin, repoRoot, "snapshot", "-o", outPat, "./tools/gsbmcodegen/fixtures/sample/...")

	for _, name := range []string{"schema_snapshot.json", "schema.yaml"} {
		a, err := os.ReadFile(filepath.Join(outDir, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		b, err := os.ReadFile(filepath.Join(outPat, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if !bytes.Equal(a, b) {
			t.Fatalf("%s differs between dir-form and pattern-form invocations", name)
		}
	}
}

// TestLoadInputsRejectsForeignDirInMixedList — a mixed dir+pattern
// invocation where the dir lives in a different module than cwd must
// fail with a clear precondition error. packages.Load can't anchor on
// two modules at once; the alternative is an opaque "no Go files in
// /abs/path" diagnostic from go/packages, which the CLI guarantees in
// its documented contract not to surface.
func TestLoadInputsRejectsForeignDirInMixedList(t *testing.T) {
	cwdMod := t.TempDir()
	if err := os.WriteFile(filepath.Join(cwdMod, "go.mod"),
		[]byte("module example.com/cwdmod\n\ngo 1.26\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cwdPkg := filepath.Join(cwdMod, "pkg")
	if err := os.MkdirAll(cwdPkg, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cwdPkg, "x.go"),
		[]byte("package pkg\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	otherMod := t.TempDir()
	if err := os.WriteFile(filepath.Join(otherMod, "go.mod"),
		[]byte("module example.com/othermod\n\ngo 1.26\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	otherPkg := filepath.Join(otherMod, "pkg")
	if err := os.MkdirAll(otherPkg, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(otherPkg, "x.go"),
		[]byte("package pkg\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Chdir(cwdMod)
	// otherPkg is a real dir → classified as argDir; "./..." is a
	// pattern. The mixed-list path routes to LoadFromPatterns, which
	// can only anchor on cwd's module. The precondition check must
	// reject otherPkg with a clear message instead of letting go/
	// packages emit its opaque "no Go files" diagnostic.
	_, err := loadInputs([]string{otherPkg, "./..."})
	if err == nil {
		t.Fatal("expected error for foreign-module dir in mixed list, got nil")
	}
	if !strings.Contains(err.Error(), "module rooted at") {
		t.Fatalf("error %q should explain the module mismatch", err.Error())
	}
}

// TestLoadInputsSkipsModuleCheckInWorkspace — running gsbmschema from a
// go.work workspace root must not trip the mixed-input same-module
// precheck. The workspace itself has no go.mod, so a strict precheck
// would refuse every mixed invocation in workspace mode even though
// go/packages can resolve patterns across all member modules. The
// guarantee here is "no precheck error"; whatever go/packages produces
// downstream is its own diagnostic.
func TestLoadInputsSkipsModuleCheckInWorkspace(t *testing.T) {
	work := t.TempDir()
	modA := filepath.Join(work, "moda")
	modB := filepath.Join(work, "modb")
	for _, m := range []string{modA, modB} {
		if err := os.MkdirAll(m, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(modA, "go.mod"),
		[]byte("module example.com/moda\n\ngo 1.26\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(modB, "go.mod"),
		[]byte("module example.com/modb\n\ngo 1.26\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(modA, "x.go"),
		[]byte("package moda\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(modB, "x.go"),
		[]byte("package modb\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "go.work"),
		[]byte("go 1.26\n\nuse (\n\t./moda\n\t./modb\n)\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Chdir(work)
	// modA is a peer-module dir; "./..." is a pattern. Under the strict
	// precheck this would have failed at moduleRoot(cwd) — workspace
	// roots have no go.mod. The carve-out lets the call reach
	// packages.Load instead.
	_, err := loadInputs([]string{modA, "./..."})
	if err != nil && strings.Contains(err.Error(), "module rooted at") {
		t.Fatalf("workspace invocation tripped the same-module precheck: %v", err)
	}
	if err != nil && strings.Contains(err.Error(), "no go.mod found") {
		t.Fatalf("workspace invocation rejected for missing go.mod at cwd: %v", err)
	}
}

// TestInWorkspaceGOWORKOff — GOWORK=off forces non-workspace behavior
// even when a go.work is present in an ancestor. Pin that branch so the
// precheck still fires for users who explicitly disable workspace mode.
func TestInWorkspaceGOWORKOff(t *testing.T) {
	work := t.TempDir()
	if err := os.WriteFile(filepath.Join(work, "go.work"),
		[]byte("go 1.26\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOWORK", "off")
	if inWorkspace(work) {
		t.Fatal("inWorkspace returned true with GOWORK=off")
	}
	t.Setenv("GOWORK", "")
	if !inWorkspace(work) {
		t.Fatal("inWorkspace returned false with go.work present and GOWORK unset")
	}
}

// TestInWorkspaceGOWORKExplicitPath — any non-empty, non-"off"/"auto"
// GOWORK value names a workspace file. inWorkspace reports true
// unconditionally so the mixed-input precheck is skipped and
// packages.Load surfaces the authoritative diagnostic — including the
// "missing file" / "wrong extension" cases, which are Go's contract
// to enforce, not ours. Treating a malformed explicit GOWORK as
// "non-workspace mode" would mask the real error behind a misleading
// module-mismatch precheck failure.
func TestInWorkspaceGOWORKExplicitPath(t *testing.T) {
	wsDir := t.TempDir()
	wsFile := filepath.Join(wsDir, "go.work")
	if err := os.WriteFile(wsFile, []byte("go 1.26\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// cwd-equivalent directory is unrelated to the workspace file's tree.
	other := t.TempDir()
	t.Setenv("GOWORK", wsFile)
	if !inWorkspace(other) {
		t.Fatalf("inWorkspace returned false with GOWORK=%s (explicit path)", wsFile)
	}
	// Missing or malformed explicit GOWORK values are Go's error to
	// raise via packages.Load. Skip the precheck so its diagnostic
	// reaches the user instead of a synthetic module-mismatch error.
	t.Setenv("GOWORK", filepath.Join(wsDir, "missing.work"))
	if !inWorkspace(other) {
		t.Fatal("inWorkspace returned false with explicit GOWORK pointing at a non-existent file; should defer to packages.Load")
	}
}

func runOK(t *testing.T, bin, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", bin, strings.Join(args, " "), err, out)
	}
}

func buildGsbmschema(t *testing.T, repoRoot string) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "gsbmschema")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	cmd := exec.Command("go", "build", "-o", bin, "./cmd/gsbmschema")
	cmd.Dir = repoRoot
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	return bin
}

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no go.mod ancestor of %s", dir)
		}
		dir = parent
	}
}
