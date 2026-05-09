package odmschema

import (
	"testing"
)

// makeSchema builds a schema with one struct named name, two fields by
// default. Tests mutate the returned schema to drive the classifier.
func makeSchema(name string, fields []*FieldDecl) *Schema {
	return &Schema{
		FmtVer: FmtVer,
		Roots: []TypeRef{
			{PkgPath: "p", Name: name},
		},
		Structs: []*StructDecl{
			{
				Type:   TypeRef{PkgPath: "p", Name: name},
				Fields: fields,
			},
		},
	}
}

// TestClassifySafeChanges covers every change in the "safe" bucket.
func TestClassifySafeChanges(t *testing.T) {
	prev := makeSchema("Offer", []*FieldDecl{
		{Name: "ID", Tag: 1, Type: "uint64", Wire: WireVarint},
	})
	curr := makeSchema("Offer", []*FieldDecl{
		{Name: "ID", Tag: 1, Type: "uint64", Wire: WireVarint},
		{Name: "Currency", Tag: 2, Type: "string", Wire: WireLengthDelim}, // safe: new field
	})
	curr.Structs[0].Reserved = []uint32{99} // safe: reserved tag added
	d := Classify(prev, curr)
	if d.MaxSeverity != SeveritySafe {
		t.Fatalf("expected safe, got %s\n%s", d.MaxSeverity, FormatDiff(d))
	}
	want := map[string]bool{"field/added": false, "reserved/added": false}
	for _, c := range d.Changes {
		want[c.Code] = true
	}
	for code, seen := range want {
		if !seen {
			t.Errorf("expected %s in diff, got %s", code, FormatDiff(d))
		}
	}
}

// TestClassifyDeprecate marking a field deprecated is safe; resurrecting
// is a warning.
func TestClassifyDeprecateAndResurrect(t *testing.T) {
	live := []*FieldDecl{{Name: "X", Tag: 1, Type: "uint64", Wire: WireVarint}}
	dep := []*FieldDecl{{Name: "X", Tag: 1, Type: "uint64", Wire: WireVarint, Deprecated: true}}

	d := Classify(makeSchema("T", live), makeSchema("T", dep))
	if d.MaxSeverity != SeveritySafe {
		t.Fatalf("expected safe (deprecate), got %s", d.MaxSeverity)
	}

	d = Classify(makeSchema("T", dep), makeSchema("T", live))
	if d.MaxSeverity != SeverityWarning {
		t.Fatalf("expected warning (resurrect), got %s", d.MaxSeverity)
	}
}

// TestClassifyResurrectIncompatible — resurrecting a deprecated field with
// a different type, wire-type, or optionality is breaking, not just a
// warning. The field is going back on the wire under the same tag and old
// blobs encoded under the previous shape would mis-decode.
func TestClassifyResurrectIncompatible(t *testing.T) {
	t.Run("resurrect with different type", func(t *testing.T) {
		dep := []*FieldDecl{{Name: "X", Tag: 1, Type: "uint64", Wire: WireVarint, Deprecated: true}}
		live := []*FieldDecl{{Name: "X", Tag: 1, Type: "string", Wire: WireLengthDelim}}
		d := Classify(makeSchema("T", dep), makeSchema("T", live))
		if d.MaxSeverity != SeverityBreaking {
			t.Fatalf("expected breaking, got %s\n%s", d.MaxSeverity, FormatDiff(d))
		}
	})

	t.Run("resurrect with different wire", func(t *testing.T) {
		// Wire shifts independently of type when the underlying primitive
		// switches between fixed and varint encodings.
		dep := []*FieldDecl{{Name: "X", Tag: 1, Type: "uint64", Wire: WireVarint, Deprecated: true}}
		live := []*FieldDecl{{Name: "X", Tag: 1, Type: "uint64", Wire: WireFixed64}}
		d := Classify(makeSchema("T", dep), makeSchema("T", live))
		if d.MaxSeverity != SeverityBreaking {
			t.Fatalf("expected breaking, got %s\n%s", d.MaxSeverity, FormatDiff(d))
		}
	})

	t.Run("resurrect with different optional", func(t *testing.T) {
		dep := []*FieldDecl{{Name: "X", Tag: 1, Type: "int64", Wire: WireVarint, Optional: false, Deprecated: true}}
		live := []*FieldDecl{{Name: "X", Tag: 1, Type: "int64", Wire: WireVarint, Optional: true}}
		d := Classify(makeSchema("T", dep), makeSchema("T", live))
		if d.MaxSeverity != SeverityBreaking {
			t.Fatalf("expected breaking, got %s\n%s", d.MaxSeverity, FormatDiff(d))
		}
	})

	t.Run("type change while staying deprecated is silent", func(t *testing.T) {
		// A deprecated field that stays deprecated is off the wire; mutating
		// its declared shape is harmless. The current behaviour (no
		// type/wire/optional change emitted) must be preserved.
		prev := []*FieldDecl{{Name: "X", Tag: 1, Type: "uint64", Wire: WireVarint, Deprecated: true}}
		curr := []*FieldDecl{{Name: "X", Tag: 1, Type: "string", Wire: WireLengthDelim, Deprecated: true}}
		d := Classify(makeSchema("T", prev), makeSchema("T", curr))
		if d.MaxSeverity != SeveritySafe {
			t.Fatalf("expected safe, got %s\n%s", d.MaxSeverity, FormatDiff(d))
		}
	})
}

// TestClassifyBreakingChanges covers each breaking case from the spec.
func TestClassifyBreakingChanges(t *testing.T) {
	t.Run("field removed", func(t *testing.T) {
		prev := makeSchema("T", []*FieldDecl{
			{Name: "X", Tag: 1, Type: "uint64", Wire: WireVarint},
		})
		curr := makeSchema("T", nil)
		d := Classify(prev, curr)
		if d.MaxSeverity != SeverityBreaking {
			t.Fatalf("got %s", d.MaxSeverity)
		}
	})

	t.Run("type changed", func(t *testing.T) {
		prev := makeSchema("T", []*FieldDecl{
			{Name: "X", Tag: 1, Type: "uint64", Wire: WireVarint},
		})
		curr := makeSchema("T", []*FieldDecl{
			{Name: "X", Tag: 1, Type: "string", Wire: WireLengthDelim},
		})
		d := Classify(prev, curr)
		if d.MaxSeverity != SeverityBreaking {
			t.Fatalf("got %s", d.MaxSeverity)
		}
	})

	t.Run("tag changed", func(t *testing.T) {
		prev := makeSchema("T", []*FieldDecl{
			{Name: "X", Tag: 1, Type: "uint64", Wire: WireVarint},
		})
		curr := makeSchema("T", []*FieldDecl{
			{Name: "X", Tag: 2, Type: "uint64", Wire: WireVarint},
		})
		d := Classify(prev, curr)
		if d.MaxSeverity != SeverityBreaking {
			t.Fatalf("got %s\n%s", d.MaxSeverity, FormatDiff(d))
		}
	})

	t.Run("reserved tag dropped", func(t *testing.T) {
		prev := makeSchema("T", nil)
		prev.Structs[0].Reserved = []uint32{42}
		curr := makeSchema("T", nil)
		d := Classify(prev, curr)
		if d.MaxSeverity != SeverityBreaking {
			t.Fatalf("got %s", d.MaxSeverity)
		}
	})

	t.Run("optional toggled required to optional", func(t *testing.T) {
		prev := makeSchema("T", []*FieldDecl{
			{Name: "X", Tag: 1, Type: "int64", Wire: WireVarint, Optional: false},
		})
		curr := makeSchema("T", []*FieldDecl{
			{Name: "X", Tag: 1, Type: "int64", Wire: WireVarint, Optional: true},
		})
		d := Classify(prev, curr)
		if d.MaxSeverity != SeverityBreaking {
			t.Fatalf("got %s\n%s", d.MaxSeverity, FormatDiff(d))
		}
		var saw bool
		for _, c := range d.Changes {
			if c.Code == "field/optional-changed" {
				saw = true
			}
		}
		if !saw {
			t.Fatalf("expected field/optional-changed, got %s", FormatDiff(d))
		}
	})

	t.Run("optional toggled optional to required", func(t *testing.T) {
		prev := makeSchema("T", []*FieldDecl{
			{Name: "X", Tag: 1, Type: "int64", Wire: WireVarint, Optional: true},
		})
		curr := makeSchema("T", []*FieldDecl{
			{Name: "X", Tag: 1, Type: "int64", Wire: WireVarint, Optional: false},
		})
		d := Classify(prev, curr)
		if d.MaxSeverity != SeverityBreaking {
			t.Fatalf("got %s\n%s", d.MaxSeverity, FormatDiff(d))
		}
	})
}

// TestAllowBreakingOverride — a struct annotated //odm:allow-breaking
// surfaces breaking changes with an Acknowledged tag, and the CI gate
// does not block.
func TestAllowBreakingOverride(t *testing.T) {
	prev := makeSchema("T", []*FieldDecl{
		{Name: "X", Tag: 1, Type: "uint64", Wire: WireVarint},
	})
	curr := makeSchema("T", nil)
	curr.Structs[0].AllowBreaking = "removing field for cleanup; tag 1 reserved"
	curr.Structs[0].Reserved = []uint32{1}

	d := Classify(prev, curr)
	if d.MaxSeverity != SeverityBreaking {
		t.Fatalf("expected breaking severity preserved, got %s", d.MaxSeverity)
	}
	report := CIDiff(prev, curr)
	if report.GateBlocks {
		t.Fatalf("CI gate should not block when every breaking change is acknowledged")
	}
}

// TestRenamePreservesTag — a rename keeps the wire-level identity, so
// it is classified safe.
func TestRenamePreservesTag(t *testing.T) {
	prev := makeSchema("T", []*FieldDecl{
		{Name: "Old", Tag: 1, Type: "uint64", Wire: WireVarint},
	})
	curr := makeSchema("T", []*FieldDecl{
		{Name: "New", Tag: 1, Type: "uint64", Wire: WireVarint},
	})
	d := Classify(prev, curr)
	if d.MaxSeverity != SeveritySafe {
		t.Fatalf("expected safe, got %s\n%s", d.MaxSeverity, FormatDiff(d))
	}
}

// TestComputeSchVerStable — same schema in same order MUST hash to the
// same uint16 across runs. A purely-cosmetic field name change MUST
// change the hash because the canonical form embeds the name.
func TestComputeSchVerStable(t *testing.T) {
	a := makeSchema("T", []*FieldDecl{
		{Name: "X", Tag: 1, Type: "uint64", Wire: WireVarint},
	})
	b := makeSchema("T", []*FieldDecl{
		{Name: "X", Tag: 1, Type: "uint64", Wire: WireVarint},
	})
	if ComputeSchVer(a) != ComputeSchVer(b) {
		t.Fatalf("schVer not stable across equal schemas")
	}
	c := makeSchema("T", []*FieldDecl{
		{Name: "Y", Tag: 1, Type: "uint64", Wire: WireVarint},
	})
	if ComputeSchVer(a) == ComputeSchVer(c) {
		t.Fatalf("schVer should change when a field is renamed (name participates in canonical form)")
	}
}
