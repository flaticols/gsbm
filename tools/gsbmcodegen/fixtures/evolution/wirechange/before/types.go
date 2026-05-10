// Package before is the "before" half of the wire-type-change scenario.
// Shipment.Carrier is `string` (WireLengthDelim). The after package
// changes the same tag's declared Go type to `int64` (WireVarint), which
// the classifier flags as breaking — old readers cannot interop with new
// writers across the change.
package before

//gsbm:root
type Shipment struct {
	Carrier string `bin:"1"`
}
