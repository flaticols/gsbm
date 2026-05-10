// Package before is the "before" half of the field-removed scenario.
// Shipment carries tags 1 and 2; the after package drops tag 2 outright,
// without marking it deprecated and without adding the tag to
// //gsbm:reserved. The append-only policy treats this as breaking.
package before

//gsbm:root
type Shipment struct {
	Carrier    string `bin:"1"`
	TrackingID string `bin:"2"`
}
