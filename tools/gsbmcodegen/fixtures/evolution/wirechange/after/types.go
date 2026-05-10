// Package after is the "after" half of the wire-type-change scenario.
// Tag 1 changed from string (WireLengthDelim) to int64 (WireVarint). The
// classifier emits both field/wire-changed and field/type-changed at
// severity breaking. The pair carries no //gsbm:allow-breaking — Task 4
// asserts the gate-blocks behavior on the unacknowledged diff and the
// flip-to-allowed behavior by setting Acknowledged programmatically.
package after

//gsbm:root
type Shipment struct {
	Carrier int64 `bin:"1"`
}
