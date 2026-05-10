package gsbmcodegen

import (
	"io"

	"go.flaticols.dev/gsbm/tools/gsbmschema"
)

// SetWarnOut swaps the package's warning sink for tests. Returns the prior
// writer so the caller can restore it.
func SetWarnOut(w io.Writer) io.Writer {
	prev := genWarnOut
	genWarnOut = w
	return prev
}

// WarnIfMaxTagExceeded exposes the internal warning helper for tests.
func WarnIfMaxTagExceeded(sd *gsbmschema.StructDecl) {
	warnIfMaxTagExceeded(sd)
}
