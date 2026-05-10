// Package laterafter is the third stage of the compat_write replace
// scenario. After the operator confirms the rollback bake-time has
// elapsed, tag 2 (Carrier) drops the compat_write annotation and stays
// plain deprecated. The diff after→laterafter emits
// field/compat-write-removed at severity breaking; the operator
// acknowledges the bake elapsed via --allow-stop-compat-write
// (DiffOptions{AllowStopCompatWrite: true}), which is the only signal
// that flips the CI gate from blocked to allowed.
package laterafter

//gsbm:root
type Shipment struct {
	ID          string `bin:"1"`
	Carrier     string `bin:"2,deprecated"`
	CarrierCode string `bin:"3"`
}
