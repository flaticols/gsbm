package graph

import (
	"reflect"
	"testing"

	"go.flaticols.dev/gsbm/storage/gsbm"
)

// TestCatalogSmokeRoundTrip is the Task-1 smoke test: it round-trips an
// empty Catalog{} and asserts the package's generated codegen wires up
// before Task 2 layers on the populated round-trip + negative tests. The
// empty-Catalog payload exercises every tag's "field-absent" branch on
// the decode path, including the high-tag (2^29 - 1) Tail field.
func TestCatalogSmokeRoundTrip(t *testing.T) {
	var w gsbm.Writer
	in := Catalog{}
	if err := in.MarshalGSBM(&w); err != nil {
		t.Fatalf("MarshalGSBM: %v", err)
	}
	r := gsbm.NewReader(w.Bytes())
	var out Catalog
	if err := out.UnmarshalGSBM(r); err != nil {
		t.Fatalf("UnmarshalGSBM: %v", err)
	}
	if !reflect.DeepEqual(in, out) {
		t.Fatalf("round-trip mismatch:\n got: %+v\nwant: %+v", out, in)
	}
}
