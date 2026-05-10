// Package after is the "after" half of the type-change scenario.
// Tag 1's Go type changed from int32 to int64; both still encode as
// WireVarint, so only field/type-changed (severity breaking) fires.
package after

//gsbm:root
type Shipment struct {
	Quantity int64 `bin:"1"`
}
