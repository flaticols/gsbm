// Package cyclebreak is a fixture exercising the //gsbm:cycle_break_via_id
// /bin:"N,id_ref" feature: a self-referential struct whose recursive
// pointer field is encoded as an ID reference rather than walking the
// type closure through the same struct again.
//
// Wire shape (see docs/spec.md §5 for the full rule):
//
//	Item.Previous (tag 3, id_ref) is omitted entirely when nil.
//	When non-nil, the field key carries the wire type of Item's bin:"1"
//	field (string → LENGTH_DELIM), and the value-payload is just that
//	field's value (the ID string). The decoder allocates a fresh *Item
//	populated only with ID; Label and Previous remain at their zero
//	values. Hydrating the rest of the chain is the caller's job.
//
// The committed *_gsbm.go siblings are byte-for-byte reproducible from
// these declarations via gsbmcodegen.Generate.
package cyclebreak

//gsbm:root
type Item struct {
	ID       string `bin:"1"`
	Label    string `bin:"2"`
	Previous *Item  `bin:"3,id_ref"`
}
