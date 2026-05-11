// Package aliasptr exercises pointer-element slices and named slice
// aliases. Resolves issue #8: the schema validator and codegen now accept
//
//   - []*T (slice of pointer to named struct) — nil mid-slice round-trips
//     via the spec §5.1 length-delim-plus-presence-byte envelope per element.
//   - type ItemList []Item — named slice alias over a value element; the
//     wire bytes are byte-identical to the underlying slice form.
//   - type ItemPtrList []*Item — named slice alias over a pointer element.
//
// The committed *_gsbm.go siblings are byte-for-byte reproducible from
// these declarations via gsbmcodegen.Generate.
package aliasptr

type Item struct {
	SKU  string  `bin:"1"`
	Note *string `bin:"2"`
}

type OptionalNote struct {
	Value string `bin:"1"`
}

type (
	ItemList    []Item
	ItemPtrList []*Item
)

//gsbm:root
type Batch struct {
	Items          []*Item         `bin:"1"`
	Optional       []*OptionalNote `bin:"2"`
	Groups         ItemList        `bin:"3"`
	OptionalGroups ItemPtrList     `bin:"4"`
}
