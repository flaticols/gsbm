package evolution

import (
	"path/filepath"
	"runtime"
	"testing"
)

// fixtureRoot returns the absolute path to this fixture package.
func fixtureRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Dir(thisFile)
}

// TestLoadSchemaBeforeAddField pins LoadSchema on a single before-package:
// it must return a schema with exactly one root, one struct (Shipment),
// two fields (tags 1 and 2), and zero issues.
func TestLoadSchemaBeforeAddField(t *testing.T) {
	res, err := LoadSchema(filepath.Join(fixtureRoot(t), "addfield", "before"))
	if err != nil {
		t.Fatalf("LoadSchema: %v", err)
	}
	if len(res.Issues) != 0 {
		t.Fatalf("unexpected issues: %v", res.Issues)
	}
	if got := len(res.Schema.Roots); got != 1 {
		t.Fatalf("roots: got %d, want 1", got)
	}
	if res.Schema.Roots[0].Name != "Shipment" {
		t.Fatalf("root name: got %q, want %q", res.Schema.Roots[0].Name, "Shipment")
	}
	if got := len(res.Schema.Structs); got != 1 {
		t.Fatalf("structs: got %d, want 1", got)
	}
	if got := len(res.Schema.Structs[0].Fields); got != 2 {
		t.Fatalf("fields: got %d, want 2", got)
	}
	tags := map[uint32]bool{}
	for _, f := range res.Schema.Structs[0].Fields {
		tags[f.Tag] = true
	}
	if !tags[1] || !tags[2] {
		t.Fatalf("missing expected tags: have %v", tags)
	}
}

// TestLoadSchemaAfterAddField pins the after-package side: same root, one
// struct, three fields (tags 1, 2, 3), zero issues. The pair is what Task
// 4's classifier diff will operate on.
func TestLoadSchemaAfterAddField(t *testing.T) {
	res, err := LoadSchema(filepath.Join(fixtureRoot(t), "addfield", "after"))
	if err != nil {
		t.Fatalf("LoadSchema: %v", err)
	}
	if len(res.Issues) != 0 {
		t.Fatalf("unexpected issues: %v", res.Issues)
	}
	if got := len(res.Schema.Structs); got != 1 {
		t.Fatalf("structs: got %d, want 1", got)
	}
	if got := len(res.Schema.Structs[0].Fields); got != 3 {
		t.Fatalf("fields: got %d, want 3", got)
	}
}

// TestLoadSchemaCompatWriteAfterFlagsField — the after side of the
// compat_write scenario must surface CompatWrite=true on tag 2 so the
// classifier's lifecycle-state machine can detect the active →
// compat_write transition. This is the load-time half of what Task 4
// asserts at diff-time.
func TestLoadSchemaCompatWriteAfterFlagsField(t *testing.T) {
	res, err := LoadSchema(filepath.Join(fixtureRoot(t), "compatwrite", "after"))
	if err != nil {
		t.Fatalf("LoadSchema: %v", err)
	}
	if len(res.Issues) != 0 {
		t.Fatalf("unexpected issues: %v", res.Issues)
	}
	var tag2Found bool
	for _, f := range res.Schema.Structs[0].Fields {
		if f.Tag != 2 {
			continue
		}
		tag2Found = true
		if !f.Deprecated || !f.CompatWrite {
			t.Fatalf("tag 2: Deprecated=%v CompatWrite=%v, want both true", f.Deprecated, f.CompatWrite)
		}
	}
	if !tag2Found {
		t.Fatalf("tag 2 missing from schema")
	}
}

// TestLoadSchemaBreakingScenariosParse — the four breaking scenarios are
// schema-valid in isolation (the breakage only surfaces when comparing
// before vs after). Each side must load cleanly so Task 4's classifier
// tests can diff them.
func TestLoadSchemaBreakingScenariosParse(t *testing.T) {
	root := fixtureRoot(t)
	scenarios := []string{"wirechange", "typechange", "tagchange", "removefield"}
	for _, s := range scenarios {
		for _, side := range []string{"before", "after"} {
			dir := filepath.Join(root, s, side)
			res, err := LoadSchema(dir)
			if err != nil {
				t.Errorf("%s/%s: LoadSchema: %v", s, side, err)
				continue
			}
			if len(res.Issues) != 0 {
				t.Errorf("%s/%s: issues: %v", s, side, res.Issues)
			}
			if len(res.Schema.Roots) != 1 || res.Schema.Roots[0].Name != "Shipment" {
				t.Errorf("%s/%s: expected one root Shipment, got %v", s, side, res.Schema.Roots)
			}
		}
	}
}
