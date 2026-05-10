// Package before is the "before" half of the compat_write replace scenario.
// Tag 2 (Carrier) is active; the after package transitions it to
// deprecated+compat_write and adds a replacement tag. The pair classifies
// as field/compat-write-added + field/added, both safe.
package before

//gsbm:root
type Shipment struct {
	ID      string `bin:"1"`
	Carrier string `bin:"2"`
}
