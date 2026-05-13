package gsbmcodegen_test

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// TestGoldenNoMarkPresent guards Task 4 of the local-presence-bitmap migration:
// generated decoders must not call gsbm.MarkPresent. The default path uses a
// stack-local bitmap and the //gsbm:track-presence path writes to an embedded
// field; in either case the sidecar surface stays untouched. The pattern is
// tolerant of import aliasing — anything ending in `.MarkPresent(` counts as
// a call and fails the guard.
func TestGoldenNoMarkPresent(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	fixturesRoot := filepath.Join(filepath.Dir(thisFile), "fixtures")

	callRE := regexp.MustCompile(`\bMarkPresent\s*\(`)

	var offenders []string
	err := filepath.Walk(fixturesRoot, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		name := info.Name()
		if !strings.HasSuffix(name, "_gsbm.go") && !strings.HasSuffix(name, "_gsbm_arena.go") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if callRE.Match(b) {
			offenders = append(offenders, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk fixtures: %v", err)
	}
	if len(offenders) > 0 {
		t.Fatalf("generated files still call MarkPresent (must use local presence bitmap):\n  %s",
			strings.Join(offenders, "\n  "))
	}
}
