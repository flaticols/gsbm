// Package after is the "after" half of the addfield evolution scenario.
// Shipment gains a third tag (Weight); the diff against before/ classifies
// as field/added (safe).
package after

//gsbm:root
type Shipment struct {
	Carrier    string `bin:"1"`
	TrackingID string `bin:"2"`
	Weight     int64  `bin:"3"`
}
