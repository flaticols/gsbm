// Package after is the "after" half of the field-removed scenario.
// Tag 2 (TrackingID) was deleted outright — not marked deprecated, not
// added to //gsbm:reserved. The classifier emits field/removed at
// severity breaking; the append-only policy demands the active →
// deprecated[+compat_write] → reserved trajectory instead.
package after

//gsbm:root
type Shipment struct {
	Carrier string `bin:"1"`
}
