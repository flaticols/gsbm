package importcollision

import (
	"reflect"
	"testing"

	"go.flaticols.dev/gsbm/storage/gsbm"
	acommon "go.flaticols.dev/gsbm/tools/gsbmcodegen/fixtures/importcollision/pkg/a/common"
	bcommon "go.flaticols.dev/gsbm/tools/gsbmcodegen/fixtures/importcollision/pkg/b/common"
)

// TestRecordRoundTrip pins the issue #22 acceptance criterion: a struct
// referencing two cross-package types that share package name `common`
// must marshal+unmarshal back to the same value. The codegen-emitted
// import block must use distinct aliases (one per real path) for the
// emitted file to even compile — passing this test confirms both the
// alias disambiguation and the field-by-field decode path.
func TestRecordRoundTrip(t *testing.T) {
	in := Record{
		A: []acommon.Value{
			{Label: "alpha", Count: 7},
			{Label: "beta", Count: -3},
		},
		B: []bcommon.Value{
			{Token: "tk", Score: 1.5},
		},
	}
	w := gsbm.NewWriter(nil)
	if err := in.MarshalGSBM(w); err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got Record
	if err := got.UnmarshalGSBM(gsbm.NewReader(w.Bytes())); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(in, got) {
		t.Fatalf("round-trip mismatch\n want: %#v\n  got: %#v", in, got)
	}
}
