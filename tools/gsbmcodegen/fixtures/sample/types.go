// Package sample is a fixture used by tools/odmcodegen tests. It exercises
// every type kind the heap-mode codegen supports: required primitives,
// optional builtins (zero-elide), required and optional named structs,
// slices of primitives, slices of structs, maps with primitive keys, and
// raw byte payloads. The committed *_odm.go siblings are byte-for-byte
// reproducible from these declarations via odmcodegen.Generate.
package sample

//odm:root
type Order struct {
	ID        string           `bin:"1"`
	Quantity  int64            `bin:"2"`
	Price     float64          `bin:"3"`
	Active    bool             `bin:"4"`
	Note      *string          `bin:"5"` // optional builtin (zero-elide eligible)
	Customer  *Customer        `bin:"6"` // optional named (no zero-elide)
	Items     []Item           `bin:"7"`
	Tags      map[string]int64 `bin:"8"`
	Payload   []byte           `bin:"9"`
	Total     Total            `bin:"10"` // required named
	Counts    []int64          `bin:"11"`
	Aliases   map[string]Label `bin:"12"` // map with named-not-struct values
	Qty       Quantity         `bin:"13"` // direct named-not-struct, varint underlying
	OptQty    *Quantity        `bin:"14"` // optional named-not-struct, varint underlying
	QtyList   []Quantity       `bin:"15"` // slice of named-not-struct, varint underlying
	OptLabel   *Label           `bin:"16"` // optional named-not-struct, string underlying
	LabelList  []Label          `bin:"17"` // slice of named-not-struct, string underlying
	OptPayload *[]byte          `bin:"18"` // optional []byte (zero-elide eligible per spec §5.1)
}

// Label is a defined-but-not-struct type with string underlying.
type Label string

// Quantity is a defined-but-not-struct type with varint (int64) underlying.
// The wire-type in the field key MUST be WireVarint, not WireLengthDelim,
// or older readers' SkipField would desync past an unknown tag.
type Quantity int64

type Customer struct {
	Name  string `bin:"1"`
	Email string `bin:"2"`
}

type Item struct {
	SKU   string `bin:"1"`
	Count int64  `bin:"2"`
}

type Total struct {
	Currency string  `bin:"1"`
	Amount   float64 `bin:"2"`
}
