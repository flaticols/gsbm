// Package after is the "after" half of the tag-change scenario.
// Field Carrier moved from tag 1 to tag 2; the classifier emits
// field/tag-changed at severity breaking.
package after

//gsbm:root
type Shipment struct {
	Carrier string `bin:"2"`
}
