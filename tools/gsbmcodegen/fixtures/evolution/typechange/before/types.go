// Package before is the "before" half of the type-change scenario.
// Shipment.Quantity is `int32`; the after package widens it to `int64`.
// Both share WireVarint, so only field/type-changed fires (not
// field/wire-changed) — that exclusivity is what distinguishes this
// scenario from wirechange/.
package before

//gsbm:root
type Shipment struct {
	Quantity int32 `bin:"1"`
}
