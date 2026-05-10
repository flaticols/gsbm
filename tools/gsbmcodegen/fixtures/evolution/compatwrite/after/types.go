// Package after is the "after" half of the compat_write replace scenario.
// Tag 2 (Carrier) is in the compat_write window — deprecated, but the
// encoder still emits it during the rollback bake. Tag 3 (CarrierCode)
// is the replacement. Diff against before/ classifies as
// field/compat-write-added (safe) + field/added (safe).
package after

//gsbm:root
type Shipment struct {
	ID          string `bin:"1"`
	Carrier     string `bin:"2,deprecated,compat_write"`
	CarrierCode string `bin:"3"`
}
