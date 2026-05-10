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
