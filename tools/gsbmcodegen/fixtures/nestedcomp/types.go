// Package nestedcomp is a fixture exercising nested composite collection
// shapes the validator and codegen accept after issue #9:
//
//   - map[K][]V    — map of slice
//   - map[K]map[K2]V — map of map
//   - []map[K]V    — slice of map
//   - map[K][]map[K2]V — 3-level nesting at MaxNestingDepth=3
//
// Each level emits its own length-delim envelope so unknown-field skip
// stays safe; the wire format itself has no nesting rules beyond that.
// The committed *_gsbm.go siblings are byte-for-byte reproducible from
// these declarations via gsbmcodegen.Generate.
package nestedcomp

//gsbm:root
type Index struct {
	IDsByGroup       map[string][]string          `bin:"1"`
	LabelsByGroup    map[string]map[string]string `bin:"2"`
	MetadataVariants []map[string]string          `bin:"3"`
	Deep             map[string][]map[string]int64 `bin:"4"`
}
