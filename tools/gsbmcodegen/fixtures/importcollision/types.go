// Package importcollision is a fixture for issue #22: a root struct that
// references two cross-package types whose Go package names collide
// (both declared `package common`). The committed *_gsbm.go siblings are
// byte-for-byte reproducible from these declarations via
// gsbmcodegen.Generate, and the disambiguating aliases the codegen picks
// must let the file compile cleanly under `go build ./...`.
//
// The fields are slices, not direct struct values, so the emitted code
// reaches typeExpr (gsbm.MakeSlice[common.Value]) and forces the codegen
// to write the qualified type name — that is the path that triggers
// addImport, where the two `common` package names collide.
package importcollision

import (
	acommon "go.flaticols.dev/gsbm/tools/gsbmcodegen/fixtures/importcollision/pkg/a/common"
	bcommon "go.flaticols.dev/gsbm/tools/gsbmcodegen/fixtures/importcollision/pkg/b/common"
)

//gsbm:root
type Record struct {
	A []acommon.Value `bin:"1"`
	B []bcommon.Value `bin:"2"`
}
