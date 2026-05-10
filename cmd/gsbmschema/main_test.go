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
