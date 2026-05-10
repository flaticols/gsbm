package sample

import (
	"testing"

	"go.flaticols.dev/gsbm/storage/gsbm"
)

// TestRenamedRoundTrip exercises the codegen-emitted MarshalGSBM/
// UnmarshalGSBM for a struct carrying a `compat_write` field. Both the old
// (LegacyCode, deprecated+compat_write) and new (RetailCode) field values
// MUST round-trip — the compat_write qualifier broadens the writer set; it
// does not affect the decode side.
func TestRenamedRoundTrip(t *testing.T) {
	in := Renamed{ID: "r-001", LegacyCode: "LX-9", RetailCode: "RX-9"}
	w := gsbm.NewWriter(nil)
	if err := in.MarshalGSBM(w); err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got Renamed
	if err := got.UnmarshalGSBM(gsbm.NewReader(w.Bytes())); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got != in {
		t.Fatalf("round-trip mismatch\n want: %#v\n  got: %#v", in, got)
	}
}

// TestRenamedCompatWriteWireHasBothTags confirms that the compat_write
// encoder dual-writes: tag 2 (LegacyCode, deprecated+compat_write) AND
// tag 3 (RetailCode) appear on the wire with their concrete values, so a
// rolled-back deploy can still find the business datum on its old tag.
//
// This is the load-bearing assertion behind the rollback claim: deprecated
// without compat_write would have stripped tag 2 from the wire.
func TestRenamedCompatWriteWireHasBothTags(t *testing.T) {
	in := Renamed{ID: "r-002", LegacyCode: "kept-on-old-tag", RetailCode: "on-new-tag"}
	w := gsbm.NewWriter(nil)
	if err := in.MarshalGSBM(w); err != nil {
		t.Fatalf("marshal: %v", err)
	}
	r := gsbm.NewReader(w.Bytes())

	seen := map[uint32]string{}
	for r.HasMore() {
		tag, wt, err := r.ReadTag()
		if err != nil {
			t.Fatalf("ReadTag: %v", err)
		}
		if wt != gsbm.WireLengthDelim {
			t.Fatalf("tag %d: unexpected wire type %v", tag, wt)
		}
		s, err := r.ReadString()
		if err != nil {
			t.Fatalf("tag %d: ReadString: %v", tag, err)
		}
		seen[tag] = s
	}
	if r.Err() != nil {
		t.Fatalf("reader err: %v", r.Err())
	}
	if seen[2] != in.LegacyCode {
		t.Fatalf("tag 2 (LegacyCode, compat_write) not on wire or wrong value: got %q", seen[2])
	}
	if seen[3] != in.RetailCode {
		t.Fatalf("tag 3 (RetailCode) not on wire or wrong value: got %q", seen[3])
	}
}

// TestRenamedCrossVersionOldSchemaPreservesLegacy simulates a rolled-back
// deploy. The new (compat_write) encoder produces a blob; an "old code"
// decoder — modeled here as a hand-rolled walk over the wire that knows
// only tag 2 (LegacyCode), as the pre-rename schema did — must still
// recover the LegacyCode value. This is the explicit guarantee the
// compat_write window provides over a bare deprecated transition.
func TestRenamedCrossVersionOldSchemaPreservesLegacy(t *testing.T) {
	in := Renamed{ID: "r-003", LegacyCode: "must-survive-rollback", RetailCode: "ignored-by-old-code"}
	w := gsbm.NewWriter(nil)
	if err := in.MarshalGSBM(w); err != nil {
		t.Fatalf("marshal: %v", err)
	}

	// Hand-rolled "old schema" decoder: knows tag 1 (ID) and tag 2
	// (LegacyCode). Anything else (notably tag 3, which old code never
	// learned about) flows through SkipField. This mirrors what the
	// codegen would emit for the pre-rename struct definition.
	type oldRenamed struct {
		ID         string
		LegacyCode string
	}
	r := gsbm.NewReader(w.Bytes())
	var old oldRenamed
	for r.HasMore() {
		tag, wt, err := r.ReadTag()
		if err != nil {
			t.Fatalf("ReadTag: %v", err)
		}
		switch tag {
		case 1:
			if wt != gsbm.WireLengthDelim {
				t.Fatalf("tag 1 wrong wire type")
			}
			s, err := r.ReadString()
			if err != nil {
				t.Fatalf("tag 1: %v", err)
			}
			old.ID = s
		case 2:
			if wt != gsbm.WireLengthDelim {
				t.Fatalf("tag 2 wrong wire type")
			}
			s, err := r.ReadString()
			if err != nil {
				t.Fatalf("tag 2: %v", err)
			}
			old.LegacyCode = s
		default:
			if err := r.SkipField(wt); err != nil {
				t.Fatalf("skip tag %d: %v", tag, err)
			}
		}
	}
	if r.Err() != nil {
		t.Fatalf("reader err: %v", r.Err())
	}
	if old.ID != in.ID {
		t.Fatalf("ID lost across rollback: want %q, got %q", in.ID, old.ID)
	}
	if old.LegacyCode != in.LegacyCode {
		t.Fatalf("LegacyCode lost across rollback (this is exactly what compat_write prevents): want %q, got %q", in.LegacyCode, old.LegacyCode)
	}
}
