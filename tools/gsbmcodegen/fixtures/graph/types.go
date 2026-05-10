// Package graph is a fixture used by tools/gsbmcodegen tests. It exercises
// composite-encoding interactions distinct from the sample fixture: a
// two-level named-struct composition (Catalog → Section → Item), a
// `map[string]<named-struct>` shape, and a tag at the upper varint
// boundary (2^29 - 1).
//
// Note on scope: the original plan called for `[]*Section`, `map[string]*Tag`,
// and `map[Code]*Tag`. The schema validator rejects these as
// `type/unsupported` (slice-of-pointer, map-value-pointer) and `map/bad-key`
// (named-string key) — see tools/gsbmschema/discover.go:398-430 and
// tools/gsbmschema/validate.go:137-143. The wire format and codegen do not
// support those shapes today, and extending them is out of scope per the
// plan's "does NOT do" clause. The fixture pivots to supported shapes; the
// nullable-element coverage gap is acknowledged in the plan as a known
// follow-up that would require schema + codegen changes.
//
// The committed *_gsbm.go siblings are byte-for-byte reproducible from
// these declarations via gsbmcodegen.Generate.
package graph

//gsbm:root
type Catalog struct {
	ID       string         `bin:"1"`
	Sections []Section      `bin:"2"`         // []<named-struct>
	Tags     map[string]Tag `bin:"3"`         // map<string, named-struct>
	Tail     EdgeMarker     `bin:"536870911"` // 2^29 - 1, the tag-boundary marker
}

type Section struct {
	Name  string `bin:"1"`
	Items []Item `bin:"2"`
}

type Item struct {
	SKU  string  `bin:"1"`
	Note *string `bin:"2"` // optional primitive inside a struct
}

type Tag struct {
	Slug   string `bin:"1"`
	Weight *int64 `bin:"2"`
}

// EdgeMarker is the type pinned at tag 2^29 - 1 on Catalog.Tail. The field
// key encodes as a 5-byte varint at this tag, the upper boundary the spec
// allows.
type EdgeMarker struct {
	Marker bool `bin:"1"`
}
