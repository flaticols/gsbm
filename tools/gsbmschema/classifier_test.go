package gsbmschema

import (
	"strings"
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

// TestClassifyDeprecateSkipsCompatWrite — going active → deprecated
// without first landing the compat_write window is a warning, not a
// pure safe change. The classifier nudges reviewers to land compat_write
// first so a rollback can still see the field on the wire.
func TestClassifyDeprecateAndResurrect(t *testing.T) {
	live := []*FieldDecl{{Name: "X", Tag: 1, Type: "uint64", Wire: WireVarint}}
	dep := []*FieldDecl{{Name: "X", Tag: 1, Type: "uint64", Wire: WireVarint, Deprecated: true}}

	d := Classify(makeSchema("T", live), makeSchema("T", dep))
	if d.MaxSeverity != SeverityWarning {
		t.Fatalf("expected warning (active→deprecated skipping compat_write), got %s\n%s", d.MaxSeverity, FormatDiff(d))
	}
	if !hasCode(d, "field/deprecated") {
		t.Fatalf("expected field/deprecated, got %s", FormatDiff(d))
	}

	d = Classify(makeSchema("T", dep), makeSchema("T", live))
	if d.MaxSeverity != SeverityWarning {
		t.Fatalf("expected warning (resurrect), got %s", d.MaxSeverity)
	}
}

// TestClassifyCompatWriteLifecycle covers the active → compat_write →
// deprecated trajectory, including each illegal jump and the gating of
// compat_write → deprecated behind --allow-stop-compat-write.
func TestClassifyCompatWriteLifecycle(t *testing.T) {
	active := []*FieldDecl{{Name: "X", Tag: 1, Type: "uint64", Wire: WireVarint}}
	compatWrite := []*FieldDecl{{Name: "X", Tag: 1, Type: "uint64", Wire: WireVarint, Deprecated: true, CompatWrite: true}}
	deprecated := []*FieldDecl{{Name: "X", Tag: 1, Type: "uint64", Wire: WireVarint, Deprecated: true}}

	t.Run("active to compat_write is safe", func(t *testing.T) {
		d := Classify(makeSchema("T", active), makeSchema("T", compatWrite))
		if d.MaxSeverity != SeveritySafe {
			t.Fatalf("expected safe, got %s\n%s", d.MaxSeverity, FormatDiff(d))
		}
		if !hasCode(d, "field/compat-write-added") {
			t.Fatalf("expected field/compat-write-added, got %s", FormatDiff(d))
		}
		if hasCode(d, "field/deprecated") {
			t.Fatalf("entering compat_write must not also emit field/deprecated: %s", FormatDiff(d))
		}
	})

	t.Run("compat_write to deprecated is breaking by default", func(t *testing.T) {
		d := Classify(makeSchema("T", compatWrite), makeSchema("T", deprecated))
		if d.MaxSeverity != SeverityBreaking {
			t.Fatalf("expected breaking, got %s\n%s", d.MaxSeverity, FormatDiff(d))
		}
		if !hasCode(d, "field/compat-write-removed") {
			t.Fatalf("expected field/compat-write-removed, got %s", FormatDiff(d))
		}
	})

	t.Run("compat_write to deprecated with allow-stop flag does not block", func(t *testing.T) {
		report := CIDiffWithOptions(makeSchema("T", compatWrite), makeSchema("T", deprecated), DiffOptions{AllowStopCompatWrite: true})
		if report.GateBlocks {
			t.Fatalf("CI gate should not block when --allow-stop-compat-write is set: %s", FormatDiff(report.Diff))
		}
		if report.Diff.MaxSeverity != SeverityBreaking {
			t.Fatalf("severity must remain breaking even when acknowledged, got %s", report.Diff.MaxSeverity)
		}
		var ack string
		for _, c := range report.Diff.Changes {
			if c.Code == "field/compat-write-removed" {
				ack = c.Acknowledged
			}
		}
		if ack == "" {
			t.Fatalf("expected acknowledged tag on field/compat-write-removed, got %s", FormatDiff(report.Diff))
		}
	})

	t.Run("compat_write to deprecated without flag blocks", func(t *testing.T) {
		report := CIDiff(makeSchema("T", compatWrite), makeSchema("T", deprecated))
		if !report.GateBlocks {
			t.Fatalf("CI gate must block compat_write→deprecated without --allow-stop-compat-write")
		}
	})

	t.Run("compat_write to deprecated cannot be acknowledged via //gsbm:allow-breaking", func(t *testing.T) {
		// The bake-window check is operator-only by design. A source-level
		// //gsbm:allow-breaking annotation must NOT satisfy it; otherwise a
		// PR could stop the dual-write without the operator's calendar-time
		// acknowledgement, defeating the safeguard.
		prev := makeSchema("T", compatWrite)
		curr := makeSchema("T", deprecated)
		curr.Structs[0].AllowBreaking = "we just want to remove it"
		report := CIDiff(prev, curr)
		if !report.GateBlocks {
			t.Fatalf("CI gate must block compat_write→deprecated even when //gsbm:allow-breaking is present: %s", FormatDiff(report.Diff))
		}
		for _, c := range report.Diff.Changes {
			if c.Code == "field/compat-write-removed" && c.Acknowledged != "" {
				t.Fatalf("field/compat-write-removed must not pick up //gsbm:allow-breaking acknowledgement, got %q", c.Acknowledged)
			}
		}
	})

	t.Run("compat_write to active is warning (resurrect)", func(t *testing.T) {
		d := Classify(makeSchema("T", compatWrite), makeSchema("T", active))
		if d.MaxSeverity != SeverityWarning {
			t.Fatalf("expected warning, got %s\n%s", d.MaxSeverity, FormatDiff(d))
		}
		if !hasCode(d, "field/resurrected") {
			t.Fatalf("expected field/resurrected, got %s", FormatDiff(d))
		}
	})

	t.Run("deprecated to compat_write is safe re-entry", func(t *testing.T) {
		d := Classify(makeSchema("T", deprecated), makeSchema("T", compatWrite))
		if d.MaxSeverity != SeveritySafe {
			t.Fatalf("expected safe, got %s\n%s", d.MaxSeverity, FormatDiff(d))
		}
		if !hasCode(d, "field/compat-write-added") {
			t.Fatalf("expected field/compat-write-added, got %s", FormatDiff(d))
		}
	})

	t.Run("compat_write stays compat_write emits no lifecycle change", func(t *testing.T) {
		d := Classify(makeSchema("T", compatWrite), makeSchema("T", compatWrite))
		for _, c := range d.Changes {
			if c.Code == "field/compat-write-added" || c.Code == "field/compat-write-removed" || c.Code == "field/deprecated" {
				t.Fatalf("steady compat_write must not emit lifecycle changes: %s", FormatDiff(d))
			}
		}
	})

	t.Run("removing a compat_write field surfaces the dual-write window", func(t *testing.T) {
		// A field deleted from the struct while still in compat_write is
		// breaking, but the detail must call out that the encoder was
		// dual-writing so the reviewer knows to land plain deprecated first.
		d := Classify(makeSchema("T", compatWrite), makeSchema("T", nil))
		if d.MaxSeverity != SeverityBreaking {
			t.Fatalf("expected breaking, got %s\n%s", d.MaxSeverity, FormatDiff(d))
		}
		var detail string
		for _, c := range d.Changes {
			if c.Code == "field/removed-deprecated" {
				detail = c.Detail
			}
		}
		if !strings.Contains(detail, "compat_write") {
			t.Fatalf("expected detail to mention compat_write, got %q", detail)
		}
	})

	t.Run("removing a plain-deprecated field steers the reviewer to //gsbm:reserved", func(t *testing.T) {
		// When a field that's already plain-deprecated gets deleted, the
		// detail must NOT tell the reviewer to "use deprecated rather than
		// delete" — the field is already deprecated. The right next step is
		// to add the tag to //gsbm:reserved so it stays unavailable.
		d := Classify(makeSchema("T", deprecated), makeSchema("T", nil))
		if d.MaxSeverity != SeverityBreaking {
			t.Fatalf("expected breaking, got %s\n%s", d.MaxSeverity, FormatDiff(d))
		}
		var detail string
		for _, c := range d.Changes {
			if c.Code == "field/removed-deprecated" {
				detail = c.Detail
			}
		}
		if detail == "" {
			t.Fatalf("expected field/removed-deprecated, got %s", FormatDiff(d))
		}
		if !strings.Contains(detail, "reserved") {
			t.Fatalf("expected detail to recommend //gsbm:reserved, got %q", detail)
		}
		if strings.Contains(detail, "use deprecated rather than delete") {
			t.Fatalf("detail must not tell the reviewer to deprecate an already-deprecated field, got %q", detail)
		}
	})

	t.Run("type change while in compat_write is breaking", func(t *testing.T) {
		// compat_write keeps the field on the wire, so a shape change is
		// not silenced like the steady-state deprecated case.
		prev := []*FieldDecl{{Name: "X", Tag: 1, Type: "uint64", Wire: WireVarint, Deprecated: true, CompatWrite: true}}
		curr := []*FieldDecl{{Name: "X", Tag: 1, Type: "string", Wire: WireLengthDelim, Deprecated: true, CompatWrite: true}}
		d := Classify(makeSchema("T", prev), makeSchema("T", curr))
		if d.MaxSeverity != SeverityBreaking {
			t.Fatalf("expected breaking, got %s\n%s", d.MaxSeverity, FormatDiff(d))
		}
	})
}

func hasCode(d Diff, code string) bool {
	for _, c := range d.Changes {
		if c.Code == code {
			return true
		}
	}
	return false
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

// TestAllowBreakingOverride — a struct annotated //gsbm:allow-breaking
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

// TestClassifyCustomMarshalerTransitions — adding `custom=Foo` to a field
// is a warning per the spec ("add custom marshaler annotation"). Removing
// or swapping the custom name is breaking because the field's emitted
// body shape on the wire changes.
func TestClassifyCustomMarshalerTransitions(t *testing.T) {
	t.Run("add custom is warning", func(t *testing.T) {
		prev := makeSchema("T", []*FieldDecl{
			{Name: "X", Tag: 1, Type: "uint64", Wire: WireVarint},
		})
		curr := makeSchema("T", []*FieldDecl{
			{Name: "X", Tag: 1, Type: "uint64", Wire: WireVarint, Custom: "PriceCodec"},
		})
		d := Classify(prev, curr)
		if d.MaxSeverity != SeverityWarning {
			t.Fatalf("expected warning, got %s\n%s", d.MaxSeverity, FormatDiff(d))
		}
		var saw bool
		for _, c := range d.Changes {
			if c.Code == "field/custom-added" {
				saw = true
			}
		}
		if !saw {
			t.Fatalf("expected field/custom-added, got %s", FormatDiff(d))
		}
	})
	t.Run("remove custom is breaking", func(t *testing.T) {
		prev := makeSchema("T", []*FieldDecl{
			{Name: "X", Tag: 1, Type: "uint64", Wire: WireVarint, Custom: "PriceCodec"},
		})
		curr := makeSchema("T", []*FieldDecl{
			{Name: "X", Tag: 1, Type: "uint64", Wire: WireVarint},
		})
		d := Classify(prev, curr)
		if d.MaxSeverity != SeverityBreaking {
			t.Fatalf("expected breaking, got %s\n%s", d.MaxSeverity, FormatDiff(d))
		}
	})
	t.Run("swap custom is breaking", func(t *testing.T) {
		prev := makeSchema("T", []*FieldDecl{
			{Name: "X", Tag: 1, Type: "uint64", Wire: WireVarint, Custom: "A"},
		})
		curr := makeSchema("T", []*FieldDecl{
			{Name: "X", Tag: 1, Type: "uint64", Wire: WireVarint, Custom: "B"},
		})
		d := Classify(prev, curr)
		if d.MaxSeverity != SeverityBreaking {
			t.Fatalf("expected breaking, got %s\n%s", d.MaxSeverity, FormatDiff(d))
		}
	})
	t.Run("custom change while deprecated is silent", func(t *testing.T) {
		prev := makeSchema("T", []*FieldDecl{
			{Name: "X", Tag: 1, Type: "uint64", Wire: WireVarint, Custom: "A", Deprecated: true},
		})
		curr := makeSchema("T", []*FieldDecl{
			{Name: "X", Tag: 1, Type: "uint64", Wire: WireVarint, Custom: "B", Deprecated: true},
		})
		d := Classify(prev, curr)
		if d.MaxSeverity != SeveritySafe {
			t.Fatalf("expected safe, got %s\n%s", d.MaxSeverity, FormatDiff(d))
		}
	})
}

// TestComputeSchemaHintStable — same schema in same order MUST hash to
// the same uint16 across runs. A purely-cosmetic field name change MUST
// change the hash because the canonical form embeds the name.
func TestComputeSchemaHintStable(t *testing.T) {
	a := makeSchema("T", []*FieldDecl{
		{Name: "X", Tag: 1, Type: "uint64", Wire: WireVarint},
	})
	b := makeSchema("T", []*FieldDecl{
		{Name: "X", Tag: 1, Type: "uint64", Wire: WireVarint},
	})
	if ComputeSchemaHint(a) != ComputeSchemaHint(b) {
		t.Fatalf("schemaHint not stable across equal schemas")
	}
	c := makeSchema("T", []*FieldDecl{
		{Name: "Y", Tag: 1, Type: "uint64", Wire: WireVarint},
	})
	if ComputeSchemaHint(a) == ComputeSchemaHint(c) {
		t.Fatalf("schemaHint should change when a field is renamed (name participates in canonical form)")
	}
}
