// Package evolution holds the paired before/after fixture sub-packages
// the schema classifier is exercised against. The package itself only
// exposes a thin loader helper used by classifier_test.go and the
// per-scenario regen_golden tests.
//
// The loader wraps gsbmschema.LoadFromDirs (the same loader the CLI
// drives) so per-struct markers — //gsbm:root, //gsbm:reserved,
// //gsbm:allow-breaking, //gsbm:deprecated — are observed identically
// to a real run. Importantly, generated _gsbm.go siblings are skipped
// by the loader (see tools/gsbmschema/loader.go:68), so committing
// goldens next to the source does not perturb schema discovery.
package evolution

import (
	"fmt"

	"go.flaticols.dev/gsbm/tools/gsbmschema"
)

// LoadSchema typechecks dir as a Go package and returns the analyzed
// schema. Issues are returned alongside the schema so callers can
// distinguish "schema is empty because the package legitimately has no
// roots" from "schema is empty because of a parse error" — both surface
// the issues list.
func LoadSchema(dir string) (*gsbmschema.AnalyzeResult, error) {
	ps, err := gsbmschema.LoadFromDirs([]string{dir})
	if err != nil {
		return nil, fmt.Errorf("load %s: %w", dir, err)
	}
	return gsbmschema.Analyze(ps), nil
}

// LoadPair loads two fixture sub-packages and returns their schemas with
// PkgPath normalized to a common synthetic value, so the classifier
// matches structs across before/after by Name alone. The two fixture
// packages live at distinct import paths (.../scenario/before vs
// .../scenario/after), and gsbmschema.Classify keys structs by
// (PkgPath, Name); without this normalization every diff would surface
// as a struct/removed + struct/added pair instead of the field-level
// changes we want to assert.
//
// Returns an error if either side fails to load or surfaces non-empty
// validation issues — paired-fixture tests are only meaningful when
// both sides are individually valid.
func LoadPair(beforeDir, afterDir string) (prev, curr *gsbmschema.Schema, err error) {
	b, err := LoadSchema(beforeDir)
	if err != nil {
		return nil, nil, fmt.Errorf("before: %w", err)
	}
	if len(b.Issues) != 0 {
		return nil, nil, fmt.Errorf("before %s issues: %s", beforeDir, gsbmschema.FormatIssues(b.Issues))
	}
	a, err := LoadSchema(afterDir)
	if err != nil {
		return nil, nil, fmt.Errorf("after: %w", err)
	}
	if len(a.Issues) != 0 {
		return nil, nil, fmt.Errorf("after %s issues: %s", afterDir, gsbmschema.FormatIssues(a.Issues))
	}
	return normalizePkgPath(b.Schema), normalizePkgPath(a.Schema), nil
}

const synthPkgPath = "evolution"

// normalizePkgPath rewrites every TypeRef.PkgPath in the schema to a
// fixed synthetic value so before/after pairs hash to identical struct
// keys under refKey().
func normalizePkgPath(s *gsbmschema.Schema) *gsbmschema.Schema {
	for i := range s.Roots {
		s.Roots[i].PkgPath = synthPkgPath
	}
	for _, sd := range s.Structs {
		sd.Type.PkgPath = synthPkgPath
	}
	return s
}
