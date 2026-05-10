// Package before is the "before" half of the tag-change scenario.
// Shipment.Carrier sits at tag 1; the after package moves the same
// (named) field to tag 2. Same name, different tag — the classifier
// detects this via the "added at new tag while old tag disappeared but
// name matches" branch and emits field/tag-changed (breaking).
package before

//gsbm:root
type Shipment struct {
	Carrier string `bin:"1"`
}
