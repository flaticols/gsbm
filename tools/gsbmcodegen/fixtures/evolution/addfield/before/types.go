// Package before is the "before" half of the addfield evolution scenario.
// Shipment exposes two fields; the after package adds a third tag. Pair
// classifies as field/added (safe).
package before

//gsbm:root
type Shipment struct {
	Carrier    string `bin:"1"`
	TrackingID string `bin:"2"`
}
