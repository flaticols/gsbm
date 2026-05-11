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
		// breaking and emits a distinct code (field/removed-compat-write),
		// not the plain field/removed-deprecated. The distinction matters
		// because the operator-only bake-time safeguard must still apply —
		// see "compat_write→removed cannot be acknowledged via //gsbm:allow-breaking".
		d := Classify(makeSchema("T", compatWrite), makeSchema("T", nil))
		if d.MaxSeverity != SeverityBreaking {
			t.Fatalf("expected breaking, got %s\n%s", d.MaxSeverity, FormatDiff(d))
		}
		var detail string
		for _, c := range d.Changes {
			if c.Code == "field/removed-compat-write" {
				detail = c.Detail
			}
		}
		if detail == "" {
			t.Fatalf("expected field/removed-compat-write, got %s", FormatDiff(d))
		}
		if !strings.Contains(detail, "compat_write") {
			t.Fatalf("expected detail to mention compat_write, got %q", detail)
		}
		if hasCode(d, "field/removed-deprecated") {
			t.Fatalf("must not emit plain field/removed-deprecated for a compat_write removal: %s", FormatDiff(d))
		}
	})

	t.Run("compat_write→removed cannot be acknowledged via //gsbm:allow-breaking", func(t *testing.T) {
		// Stacking compat_write→removed in a single PR must not bypass the
		// operator-only bake-time safeguard. The source-level
		// //gsbm:allow-breaking annotation must NOT acknowledge
		// field/removed-compat-write; the operator must invoke
		// --allow-stop-compat-write to stop the dual-write first.
		prev := makeSchema("T", compatWrite)
		curr := makeSchema("T", nil)
		curr.Structs[0].AllowBreaking = "removing field"
		curr.Structs[0].Reserved = []uint32{1}
		report := CIDiff(prev, curr)
		if !report.GateBlocks {
			t.Fatalf("CI gate must block compat_write→removed even when //gsbm:allow-breaking is present: %s", FormatDiff(report.Diff))
		}
		for _, c := range report.Diff.Changes {
			if c.Code == "field/removed-compat-write" && c.Acknowledged != "" {
				t.Fatalf("field/removed-compat-write must not pick up //gsbm:allow-breaking acknowledgement, got %q", c.Acknowledged)
			}
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
	// discover.go skips fillTypeShape for custom-codec fields so fd.Wire
	// stays empty. Toggling `custom=` on/off then flips Wire between "" and
	// e.g. "varint"; the wire-changed code must not fire because the
	// custom-add/remove event already conveys the wire-shape change.
	t.Run("add custom does not emit field/wire-changed", func(t *testing.T) {
		prev := makeSchema("T", []*FieldDecl{
			{Name: "X", Tag: 1, Type: "uint64", Wire: WireVarint},
		})
		curr := makeSchema("T", []*FieldDecl{
			{Name: "X", Tag: 1, Type: "time.Time", Wire: "", Custom: "TimeUnixNano"},
		})
		d := Classify(prev, curr)
		for _, c := range d.Changes {
			if c.Code == "field/wire-changed" {
				t.Fatalf("unexpected field/wire-changed: %s", FormatDiff(d))
			}
		}
	})
	t.Run("remove custom does not emit field/wire-changed", func(t *testing.T) {
		prev := makeSchema("T", []*FieldDecl{
			{Name: "X", Tag: 1, Type: "time.Time", Wire: "", Custom: "TimeUnixNano"},
		})
		curr := makeSchema("T", []*FieldDecl{
			{Name: "X", Tag: 1, Type: "uint64", Wire: WireVarint},
		})
		d := Classify(prev, curr)
		for _, c := range d.Changes {
			if c.Code == "field/wire-changed" {
				t.Fatalf("unexpected field/wire-changed: %s", FormatDiff(d))
			}
		}
	})
}

// TestClassifyCycleBreakTransitions — toggling the cycle-break flag on a
// field changes its on-wire body from a nested struct body to a leaf
// scalar (the target's bin:"1" ID) or vice versa. Old readers and new
// readers cannot interop across the toggle, so both directions are
// breaking. While the field stays deprecated in both snapshots the body
// is off the wire and the toggle is silenced, matching how
// field/type-changed and field/wire-changed are handled.
func TestClassifyCycleBreakTransitions(t *testing.T) {
	t.Run("adding id_ref is breaking", func(t *testing.T) {
		prev := makeSchema("T", []*FieldDecl{
			{Name: "Previous", Tag: 3, Type: "*p.T", Wire: WireLengthDelim},
		})
		curr := makeSchema("T", []*FieldDecl{
			{Name: "Previous", Tag: 3, Type: "*p.T", Wire: WireLengthDelim, CycleBreak: true},
		})
		d := Classify(prev, curr)
		if d.MaxSeverity != SeverityBreaking {
			t.Fatalf("expected breaking, got %s\n%s", d.MaxSeverity, FormatDiff(d))
		}
		if !hasCode(d, "field/cycle-break-added") {
			t.Fatalf("expected field/cycle-break-added, got %s", FormatDiff(d))
		}
	})
	t.Run("removing id_ref is breaking", func(t *testing.T) {
		prev := makeSchema("T", []*FieldDecl{
			{Name: "Previous", Tag: 3, Type: "*p.T", Wire: WireLengthDelim, CycleBreak: true},
		})
		curr := makeSchema("T", []*FieldDecl{
			{Name: "Previous", Tag: 3, Type: "*p.T", Wire: WireLengthDelim},
		})
		d := Classify(prev, curr)
		if d.MaxSeverity != SeverityBreaking {
			t.Fatalf("expected breaking, got %s\n%s", d.MaxSeverity, FormatDiff(d))
		}
		if !hasCode(d, "field/cycle-break-removed") {
			t.Fatalf("expected field/cycle-break-removed, got %s", FormatDiff(d))
		}
	})
	t.Run("steady cycle-break emits no change", func(t *testing.T) {
		prev := makeSchema("T", []*FieldDecl{
			{Name: "Previous", Tag: 3, Type: "*p.T", Wire: WireLengthDelim, CycleBreak: true},
		})
		curr := makeSchema("T", []*FieldDecl{
			{Name: "Previous", Tag: 3, Type: "*p.T", Wire: WireLengthDelim, CycleBreak: true},
		})
		d := Classify(prev, curr)
		if hasCode(d, "field/cycle-break-added") || hasCode(d, "field/cycle-break-removed") {
			t.Fatalf("steady cycle-break must not emit a transition: %s", FormatDiff(d))
		}
	})
	t.Run("toggle while deprecated is silent", func(t *testing.T) {
		prev := makeSchema("T", []*FieldDecl{
			{Name: "Previous", Tag: 3, Type: "*p.T", Wire: WireLengthDelim, Deprecated: true},
		})
		curr := makeSchema("T", []*FieldDecl{
			{Name: "Previous", Tag: 3, Type: "*p.T", Wire: WireLengthDelim, Deprecated: true, CycleBreak: true},
		})
		d := Classify(prev, curr)
		if hasCode(d, "field/cycle-break-added") || hasCode(d, "field/cycle-break-removed") {
			t.Fatalf("toggle while deprecated must be silent: %s", FormatDiff(d))
		}
	})
	t.Run("toggle while compat_write is breaking", func(t *testing.T) {
		// compat_write keeps the field on the wire during the rollback bake,
		// so the body-shape change is observable and must surface.
		prev := makeSchema("T", []*FieldDecl{
			{Name: "Previous", Tag: 3, Type: "*p.T", Wire: WireLengthDelim, Deprecated: true, CompatWrite: true},
		})
		curr := makeSchema("T", []*FieldDecl{
			{Name: "Previous", Tag: 3, Type: "*p.T", Wire: WireLengthDelim, Deprecated: true, CompatWrite: true, CycleBreak: true},
		})
		d := Classify(prev, curr)
		if d.MaxSeverity != SeverityBreaking {
			t.Fatalf("expected breaking while in compat_write window, got %s\n%s", d.MaxSeverity, FormatDiff(d))
		}
		if !hasCode(d, "field/cycle-break-added") {
			t.Fatalf("expected field/cycle-break-added, got %s", FormatDiff(d))
		}
	})
	t.Run("acknowledged via allow-breaking", func(t *testing.T) {
		prev := makeSchema("T", []*FieldDecl{
			{Name: "Previous", Tag: 3, Type: "*p.T", Wire: WireLengthDelim},
		})
		curr := makeSchema("T", []*FieldDecl{
			{Name: "Previous", Tag: 3, Type: "*p.T", Wire: WireLengthDelim, CycleBreak: true},
		})
		curr.Structs[0].AllowBreaking = "switching Previous to ID reference"
		report := CIDiff(prev, curr)
		if report.GateBlocks {
			t.Fatalf("CI gate should not block when //gsbm:allow-breaking is set:\n%s", FormatDiff(report.Diff))
		}
	})
}

// TestClassifyIDRefOpaqueTargetIDTypeChange — the wire shape of an
// id_ref field is the target's bin:"1" field encoded as a leaf scalar.
// When the target is opaque it contributes no field-level snapshot, so
// the change must surface on the referencing field instead. Discover
// records the resolved id-field wire type on the referencing field's
// snapshot, and the classifier's existing field/wire-changed branch
// flags the swap as breaking — closing the silent-wire-flip hole for
// opaque targets.
func TestClassifyIDRefOpaqueTargetIDTypeChange(t *testing.T) {
	prev := makeSchema("Holder", []*FieldDecl{
		{Name: "Ref", Tag: 1, Type: "*p.Target", Wire: WireLengthDelim, CycleBreak: true},
	})
	curr := makeSchema("Holder", []*FieldDecl{
		{Name: "Ref", Tag: 1, Type: "*p.Target", Wire: WireVarint, CycleBreak: true},
	})
	d := Classify(prev, curr)
	if d.MaxSeverity != SeverityBreaking {
		t.Fatalf("expected breaking on id_ref target wire flip, got %s\n%s", d.MaxSeverity, FormatDiff(d))
	}
	if !hasCode(d, "field/wire-changed") {
		t.Fatalf("expected field/wire-changed, got %s", FormatDiff(d))
	}
}

// TestClassifyIDRefOpaqueTargetSameWireTypeChange — same-wire-class flips
// of the resolved id_ref ID type (int32→int64 stays varint, string→[]byte
// stays length-delim) MUST still surface as breaking. The opaque-target
// safeguard records the resolved ID's shape on the referencing field's
// Type so the classifier's existing field/type-changed branch fires even
// when fd.Wire is unchanged.
func TestClassifyIDRefOpaqueTargetSameWireTypeChange(t *testing.T) {
	t.Run("int32 to int64", func(t *testing.T) {
		prev := makeSchema("Holder", []*FieldDecl{
			{Name: "Ref", Tag: 1, Type: "*p.Target/id:int32", Wire: WireVarint, CycleBreak: true},
		})
		curr := makeSchema("Holder", []*FieldDecl{
			{Name: "Ref", Tag: 1, Type: "*p.Target/id:int64", Wire: WireVarint, CycleBreak: true},
		})
		d := Classify(prev, curr)
		if d.MaxSeverity != SeverityBreaking {
			t.Fatalf("expected breaking on int32→int64 id flip, got %s\n%s", d.MaxSeverity, FormatDiff(d))
		}
		if !hasCode(d, "field/type-changed") {
			t.Fatalf("expected field/type-changed, got %s", FormatDiff(d))
		}
	})
	t.Run("string to bytes", func(t *testing.T) {
		prev := makeSchema("Holder", []*FieldDecl{
			{Name: "Ref", Tag: 1, Type: "*p.Target/id:string", Wire: WireLengthDelim, CycleBreak: true},
		})
		curr := makeSchema("Holder", []*FieldDecl{
			{Name: "Ref", Tag: 1, Type: "*p.Target/id:[]uint8", Wire: WireLengthDelim, CycleBreak: true},
		})
		d := Classify(prev, curr)
		if d.MaxSeverity != SeverityBreaking {
			t.Fatalf("expected breaking on string→[]byte id flip, got %s\n%s", d.MaxSeverity, FormatDiff(d))
		}
		if !hasCode(d, "field/type-changed") {
			t.Fatalf("expected field/type-changed, got %s", FormatDiff(d))
		}
	})
}

// TestClassifyMapKeyUnderlyingChange — a named map-key (`type Code
// string`) whose underlying primitive flips (string → int64) changes
// the on-wire encoding of every key, even when the named identifier
// itself is unchanged. The classifier MUST flag this as breaking so
// reviewers cannot land it silently. Identical Underlying must produce
// no diff.
func TestClassifyMapKeyUnderlyingChange(t *testing.T) {
	t.Run("same name same underlying is no diff", func(t *testing.T) {
		prev := makeSchema("T", []*FieldDecl{
			{Name: "M", Tag: 1, Type: "map[p.Code(string)]int64", Wire: WireLengthDelim,
				MapKey: "p.Code(string)", MapValue: "int64", MapKeyUnderlying: "string"},
		})
		curr := makeSchema("T", []*FieldDecl{
			{Name: "M", Tag: 1, Type: "map[p.Code(string)]int64", Wire: WireLengthDelim,
				MapKey: "p.Code(string)", MapValue: "int64", MapKeyUnderlying: "string"},
		})
		d := Classify(prev, curr)
		if d.MaxSeverity != SeveritySafe {
			t.Fatalf("expected safe (no diff), got %s\n%s", d.MaxSeverity, FormatDiff(d))
		}
	})

	t.Run("same name different underlying is breaking", func(t *testing.T) {
		// Pin fd.Type identical on both sides so the new
		// field/map-key-underlying-changed code is exercised in isolation
		// from the pre-existing field/type-changed branch.
		prev := makeSchema("T", []*FieldDecl{
			{Name: "M", Tag: 1, Type: "map[p.Code]int64", Wire: WireLengthDelim,
				MapKey: "p.Code", MapValue: "int64", MapKeyUnderlying: "string"},
		})
		curr := makeSchema("T", []*FieldDecl{
			{Name: "M", Tag: 1, Type: "map[p.Code]int64", Wire: WireLengthDelim,
				MapKey: "p.Code", MapValue: "int64", MapKeyUnderlying: "int64"},
		})
		d := Classify(prev, curr)
		if d.MaxSeverity != SeverityBreaking {
			t.Fatalf("expected breaking, got %s\n%s", d.MaxSeverity, FormatDiff(d))
		}
		if !hasCode(d, "field/map-key-underlying-changed") {
			t.Fatalf("expected field/map-key-underlying-changed, got %s", FormatDiff(d))
		}
	})

	t.Run("named to raw primitive does not fire underlying-changed", func(t *testing.T) {
		// Replacing `map[Code]V` with `map[string]V` removes the named
		// type. MapKeyUnderlying goes from "string" to "" — the named
		// wrapper is gone, but the wire bytes for the key are identical
		// because the underlying primitive is the same. The schema-type
		// change is already surfaced by field/type-changed; firing
		// field/map-key-underlying-changed here would claim "wire
		// encoding changes; old blobs cannot be decoded", which is
		// untrue for this transition.
		prev := makeSchema("T", []*FieldDecl{
			{Name: "M", Tag: 1, Type: "map[p.Code(string)]int64", Wire: WireLengthDelim,
				MapKey: "p.Code(string)", MapValue: "int64", MapKeyUnderlying: "string"},
		})
		curr := makeSchema("T", []*FieldDecl{
			{Name: "M", Tag: 1, Type: "map[string]int64", Wire: WireLengthDelim,
				MapKey: "string", MapValue: "int64"},
		})
		d := Classify(prev, curr)
		if hasCode(d, "field/map-key-underlying-changed") {
			t.Fatalf("named→raw key transition must not fire field/map-key-underlying-changed (wire bytes unchanged; covered by field/type-changed): %s", FormatDiff(d))
		}
		// Pin the documented fallback: field/type-changed must still
		// surface the schema-type change at breaking severity, so a
		// future regression cannot make the transition silent.
		if !hasCode(d, "field/type-changed") {
			t.Fatalf("expected field/type-changed to surface named→raw key transition: %s", FormatDiff(d))
		}
		if d.MaxSeverity != SeverityBreaking {
			t.Fatalf("expected breaking severity, got %s\n%s", d.MaxSeverity, FormatDiff(d))
		}
	})

	t.Run("raw to named primitive does not fire underlying-changed", func(t *testing.T) {
		// Symmetric to the named→raw case: adding the named wrapper
		// around an existing raw key does not change the wire bytes
		// when the underlying primitive is identical. field/type-changed
		// covers the schema-type change.
		prev := makeSchema("T", []*FieldDecl{
			{Name: "M", Tag: 1, Type: "map[string]int64", Wire: WireLengthDelim,
				MapKey: "string", MapValue: "int64"},
		})
		curr := makeSchema("T", []*FieldDecl{
			{Name: "M", Tag: 1, Type: "map[p.Code(string)]int64", Wire: WireLengthDelim,
				MapKey: "p.Code(string)", MapValue: "int64", MapKeyUnderlying: "string"},
		})
		d := Classify(prev, curr)
		if hasCode(d, "field/map-key-underlying-changed") {
			t.Fatalf("raw→named key transition must not fire field/map-key-underlying-changed (wire bytes unchanged; covered by field/type-changed): %s", FormatDiff(d))
		}
		if !hasCode(d, "field/type-changed") {
			t.Fatalf("expected field/type-changed to surface raw→named key transition: %s", FormatDiff(d))
		}
		if d.MaxSeverity != SeverityBreaking {
			t.Fatalf("expected breaking severity, got %s\n%s", d.MaxSeverity, FormatDiff(d))
		}
	})

	t.Run("scalar to map does not fire underlying-changed", func(t *testing.T) {
		// A field flipping from a scalar to a named-keyed map sets
		// MapKeyUnderlying from "" to a primitive name; the rule must not
		// fire because field/type-changed already covers the shape flip.
		prev := makeSchema("T", []*FieldDecl{
			{Name: "M", Tag: 1, Type: "int64", Wire: WireVarint},
		})
		curr := makeSchema("T", []*FieldDecl{
			{Name: "M", Tag: 1, Type: "map[p.Code]int64", Wire: WireLengthDelim,
				MapKey: "p.Code", MapValue: "int64", MapKeyUnderlying: "string"},
		})
		d := Classify(prev, curr)
		if hasCode(d, "field/map-key-underlying-changed") {
			t.Fatalf("scalar→map must not fire field/map-key-underlying-changed (covered by field/type-changed): %s", FormatDiff(d))
		}
		if !hasCode(d, "field/type-changed") {
			t.Fatalf("expected field/type-changed to surface scalar→map transition: %s", FormatDiff(d))
		}
		if d.MaxSeverity != SeverityBreaking {
			t.Fatalf("expected breaking severity, got %s\n%s", d.MaxSeverity, FormatDiff(d))
		}
	})

	t.Run("underlying change while deprecated is silent", func(t *testing.T) {
		prev := makeSchema("T", []*FieldDecl{
			{Name: "M", Tag: 1, Type: "map[p.Code]int64", Wire: WireLengthDelim,
				MapKey: "p.Code", MapValue: "int64", MapKeyUnderlying: "string", Deprecated: true},
		})
		curr := makeSchema("T", []*FieldDecl{
			{Name: "M", Tag: 1, Type: "map[p.Code]int64", Wire: WireLengthDelim,
				MapKey: "p.Code", MapValue: "int64", MapKeyUnderlying: "int64", Deprecated: true},
		})
		d := Classify(prev, curr)
		if hasCode(d, "field/map-key-underlying-changed") {
			t.Fatalf("change must be silenced while deprecated, got %s", FormatDiff(d))
		}
	})
}

// TestDiscoverMapKeyUnderlyingPopulated — the discover pipeline MUST
// populate MapKeyUnderlying for named primitive keys (so the classifier
// rule above has a value to compare against) and leave it empty for
// raw-primitive keys.
func TestDiscoverMapKeyUnderlyingPopulated(t *testing.T) {
	ps, err := ParseSource("p", []string{`
package p

type Code string

//gsbm:root
type Counts struct {
	ByCode   map[Code]int64   ` + "`bin:\"1\"`" + `
	ByString map[string]int64 ` + "`bin:\"2\"`" + `
}
`})
	if err != nil {
		t.Fatal(err)
	}
	res := Analyze(ps)
	if len(res.Issues) != 0 {
		t.Fatalf("issues: %s", FormatIssues(res.Issues))
	}
	var counts *StructDecl
	for _, sd := range res.Schema.Structs {
		if sd.Type.Name == "Counts" {
			counts = sd
			break
		}
	}
	if counts == nil {
		t.Fatal("Counts struct not found in schema")
	}
	if len(counts.Fields) != 2 {
		t.Fatalf("expected 2 fields, got %d", len(counts.Fields))
	}
	byCode := counts.Fields[0]
	if byCode.MapKeyUnderlying != "string" {
		t.Errorf("ByCode.MapKeyUnderlying = %q, want %q", byCode.MapKeyUnderlying, "string")
	}
	byString := counts.Fields[1]
	if byString.MapKeyUnderlying != "" {
		t.Errorf("ByString.MapKeyUnderlying = %q, want empty (raw-primitive key)", byString.MapKeyUnderlying)
	}
}

// TestClassifyNestedCompositeShapeChange — issue #9: a structural
// change at any depth of a nested composite shape flips fd.Type and
// MUST surface as field/type-changed (breaking). The classifier's
// shape comparison is a string equality on fd.Type, which shapeOf
// renders recursively, so the same diff machinery covers depth-1 and
// depth-N alike. These tests pin the contract for the two canonical
// cases called out in the plan:
//
//   - map[K][]V → map[K]V  (drops a composite level: wire shape changes)
//   - map[K][]V → map[K][]V' (leaf type changes at depth 2)
func TestClassifyNestedCompositeShapeChange(t *testing.T) {
	t.Run("drop slice level is breaking", func(t *testing.T) {
		prev := makeSchema("T", []*FieldDecl{
			{Name: "M", Tag: 1, Type: "map[string][]string", Wire: WireLengthDelim,
				MapKey: "string", MapValue: "[]string"},
		})
		curr := makeSchema("T", []*FieldDecl{
			{Name: "M", Tag: 1, Type: "map[string]string", Wire: WireLengthDelim,
				MapKey: "string", MapValue: "string"},
		})
		d := Classify(prev, curr)
		if d.MaxSeverity != SeverityBreaking {
			t.Fatalf("expected breaking, got %s\n%s", d.MaxSeverity, FormatDiff(d))
		}
		if !hasCode(d, "field/type-changed") {
			t.Fatalf("expected field/type-changed, got %s", FormatDiff(d))
		}
	})

	t.Run("leaf change at depth 2 is breaking", func(t *testing.T) {
		prev := makeSchema("T", []*FieldDecl{
			{Name: "M", Tag: 1, Type: "map[string][]string", Wire: WireLengthDelim,
				MapKey: "string", MapValue: "[]string"},
		})
		curr := makeSchema("T", []*FieldDecl{
			{Name: "M", Tag: 1, Type: "map[string][]int64", Wire: WireLengthDelim,
				MapKey: "string", MapValue: "[]int64"},
		})
		d := Classify(prev, curr)
		if d.MaxSeverity != SeverityBreaking {
			t.Fatalf("expected breaking, got %s\n%s", d.MaxSeverity, FormatDiff(d))
		}
		if !hasCode(d, "field/type-changed") {
			t.Fatalf("expected field/type-changed, got %s", FormatDiff(d))
		}
	})

	t.Run("leaf change at depth 3 is breaking", func(t *testing.T) {
		// 3-deep map of slice of map is the practical ceiling per
		// MaxNestingDepth=3. A leaf change here must still flip fd.Type
		// — that's what gives the classifier its "any structural change
		// at any depth" guarantee.
		prev := makeSchema("T", []*FieldDecl{
			{Name: "M", Tag: 1, Type: "map[string][]map[string]int64", Wire: WireLengthDelim,
				MapKey: "string", MapValue: "[]map[string]int64"},
		})
		curr := makeSchema("T", []*FieldDecl{
			{Name: "M", Tag: 1, Type: "map[string][]map[string]uint64", Wire: WireLengthDelim,
				MapKey: "string", MapValue: "[]map[string]uint64"},
		})
		d := Classify(prev, curr)
		if d.MaxSeverity != SeverityBreaking {
			t.Fatalf("expected breaking, got %s\n%s", d.MaxSeverity, FormatDiff(d))
		}
		if !hasCode(d, "field/type-changed") {
			t.Fatalf("expected field/type-changed, got %s", FormatDiff(d))
		}
	})

	t.Run("identical nested shape is no diff", func(t *testing.T) {
		fields := []*FieldDecl{
			{Name: "M", Tag: 1, Type: "map[string][]map[string]int64", Wire: WireLengthDelim,
				MapKey: "string", MapValue: "[]map[string]int64"},
		}
		d := Classify(makeSchema("T", fields), makeSchema("T", fields))
		if d.MaxSeverity != SeveritySafe {
			t.Fatalf("expected safe (no diff), got %s\n%s", d.MaxSeverity, FormatDiff(d))
		}
	})
}

// TestClassifyNamedSliceAliasRename — renaming a named slice alias
// while keeping the underlying slice element unchanged is wire-stable
// and must classify as safe (field/alias-renamed), not as
// field/type-changed. The wire-bytes-relevant shape lives in fd.Type
// (the underlying slice form) which is identical across the rename; the
// alias identity flips in fd.AliasType.
func TestClassifyNamedSliceAliasRename(t *testing.T) {
	prev := makeSchema("T", []*FieldDecl{
		{Name: "Groups", Tag: 1, Type: "[]p.Item", Wire: WireLengthDelim, Elem: "p.Item",
			AliasType: &TypeRef{PkgPath: "p", Name: "ItemList",
				Underlying: &TypeRef{PkgPath: "p", Name: "Item"}}},
	})
	curr := makeSchema("T", []*FieldDecl{
		{Name: "Groups", Tag: 1, Type: "[]p.Item", Wire: WireLengthDelim, Elem: "p.Item",
			AliasType: &TypeRef{PkgPath: "p", Name: "Items",
				Underlying: &TypeRef{PkgPath: "p", Name: "Item"}}},
	})
	d := Classify(prev, curr)
	if d.MaxSeverity != SeveritySafe {
		t.Fatalf("expected safe on alias rename (same underlying), got %s\n%s", d.MaxSeverity, FormatDiff(d))
	}
	if !hasCode(d, "field/alias-renamed") {
		t.Fatalf("expected field/alias-renamed, got %s", FormatDiff(d))
	}
	if hasCode(d, "field/type-changed") {
		t.Fatalf("rename with same underlying must not fire field/type-changed: %s", FormatDiff(d))
	}
}

// TestClassifyNamedSliceAliasUnderlyingChange — swapping the underlying
// element type (`type ItemList []Item` → `type ItemList []OtherItem`)
// flips the wire bytes and must classify as breaking. The diff surfaces
// via field/type-changed because fd.Type carries the underlying slice
// shape, which is what we want — old blobs cannot decode under the new
// schema.
func TestClassifyNamedSliceAliasUnderlyingChange(t *testing.T) {
	prev := makeSchema("T", []*FieldDecl{
		{Name: "Groups", Tag: 1, Type: "[]p.Item", Wire: WireLengthDelim, Elem: "p.Item",
			AliasType: &TypeRef{PkgPath: "p", Name: "ItemList",
				Underlying: &TypeRef{PkgPath: "p", Name: "Item"}}},
	})
	curr := makeSchema("T", []*FieldDecl{
		{Name: "Groups", Tag: 1, Type: "[]p.OtherItem", Wire: WireLengthDelim, Elem: "p.OtherItem",
			AliasType: &TypeRef{PkgPath: "p", Name: "ItemList",
				Underlying: &TypeRef{PkgPath: "p", Name: "OtherItem"}}},
	})
	d := Classify(prev, curr)
	if d.MaxSeverity != SeverityBreaking {
		t.Fatalf("expected breaking on underlying change, got %s\n%s", d.MaxSeverity, FormatDiff(d))
	}
	if !hasCode(d, "field/type-changed") {
		t.Fatalf("expected field/type-changed (the underlying slice shape flipped), got %s", FormatDiff(d))
	}
	if hasCode(d, "field/alias-renamed") {
		t.Fatalf("alias name unchanged; field/alias-renamed must not fire: %s", FormatDiff(d))
	}
}

// TestClassifyNamedSliceAliasRawSwap — declaring a field as the
// underlying slice directly (`Groups []Item`) and then introducing the
// named alias (`Groups ItemList` where `type ItemList []Item`) leaves
// the wire bytes identical. Symmetric for the reverse direction. Both
// transitions classify as safe via field/alias-added / field/alias-removed.
func TestClassifyNamedSliceAliasRawSwap(t *testing.T) {
	raw := []*FieldDecl{
		{Name: "Groups", Tag: 1, Type: "[]p.Item", Wire: WireLengthDelim, Elem: "p.Item"},
	}
	aliased := []*FieldDecl{
		{Name: "Groups", Tag: 1, Type: "[]p.Item", Wire: WireLengthDelim, Elem: "p.Item",
			AliasType: &TypeRef{PkgPath: "p", Name: "ItemList",
				Underlying: &TypeRef{PkgPath: "p", Name: "Item"}}},
	}
	t.Run("raw to aliased", func(t *testing.T) {
		d := Classify(makeSchema("T", raw), makeSchema("T", aliased))
		if d.MaxSeverity != SeveritySafe {
			t.Fatalf("expected safe, got %s\n%s", d.MaxSeverity, FormatDiff(d))
		}
		if !hasCode(d, "field/alias-added") {
			t.Fatalf("expected field/alias-added, got %s", FormatDiff(d))
		}
	})
	t.Run("aliased to raw", func(t *testing.T) {
		d := Classify(makeSchema("T", aliased), makeSchema("T", raw))
		if d.MaxSeverity != SeveritySafe {
			t.Fatalf("expected safe, got %s\n%s", d.MaxSeverity, FormatDiff(d))
		}
		if !hasCode(d, "field/alias-removed") {
			t.Fatalf("expected field/alias-removed, got %s", FormatDiff(d))
		}
	})
}

// TestClassifyAliasAddedWithTypeChange — when an alias is introduced
// AND the underlying slice element shape changes in the same diff (e.g.
// `Groups []Item` → `Groups ItemPtrList` where `type ItemPtrList []*Item`),
// the breaking signal must come from field/type-changed alone. The
// field/alias-added branch must NOT fire here because its detail text
// claims "wire bytes unchanged", which would be false. Symmetric check
// for alias-removed.
func TestClassifyAliasAddedWithTypeChange(t *testing.T) {
	t.Run("alias added with type change", func(t *testing.T) {
		prev := makeSchema("T", []*FieldDecl{
			{Name: "Groups", Tag: 1, Type: "[]p.Item", Wire: WireLengthDelim, Elem: "p.Item"},
		})
		curr := makeSchema("T", []*FieldDecl{
			{Name: "Groups", Tag: 1, Type: "[]*p.Item", Wire: WireLengthDelim, Elem: "*p.Item",
				AliasType: &TypeRef{PkgPath: "p", Name: "ItemPtrList",
					Underlying: &TypeRef{PkgPath: "p", Name: "*p.Item"}}},
		})
		d := Classify(prev, curr)
		if d.MaxSeverity != SeverityBreaking {
			t.Fatalf("expected breaking, got %s\n%s", d.MaxSeverity, FormatDiff(d))
		}
		if !hasCode(d, "field/type-changed") {
			t.Fatalf("expected field/type-changed, got %s", FormatDiff(d))
		}
		if hasCode(d, "field/alias-added") {
			t.Fatalf("field/alias-added must not fire when fd.Type also changed (wire bytes are not unchanged): %s", FormatDiff(d))
		}
	})
	t.Run("alias removed with type change", func(t *testing.T) {
		prev := makeSchema("T", []*FieldDecl{
			{Name: "Groups", Tag: 1, Type: "[]*p.Item", Wire: WireLengthDelim, Elem: "*p.Item",
				AliasType: &TypeRef{PkgPath: "p", Name: "ItemPtrList",
					Underlying: &TypeRef{PkgPath: "p", Name: "*p.Item"}}},
		})
		curr := makeSchema("T", []*FieldDecl{
			{Name: "Groups", Tag: 1, Type: "[]p.Item", Wire: WireLengthDelim, Elem: "p.Item"},
		})
		d := Classify(prev, curr)
		if d.MaxSeverity != SeverityBreaking {
			t.Fatalf("expected breaking, got %s\n%s", d.MaxSeverity, FormatDiff(d))
		}
		if !hasCode(d, "field/type-changed") {
			t.Fatalf("expected field/type-changed, got %s", FormatDiff(d))
		}
		if hasCode(d, "field/alias-removed") {
			t.Fatalf("field/alias-removed must not fire when fd.Type also changed: %s", FormatDiff(d))
		}
	})
}

// TestDiscoverNamedSliceAliasPopulatesAliasType — the discover pipeline
// MUST populate fd.AliasType for named slice aliases used as the
// top-level field type, with the alias identifier in Name and the slice
// element type captured under Underlying. Non-aliased slices and named
// non-slice fields leave AliasType nil.
func TestDiscoverNamedSliceAliasPopulatesAliasType(t *testing.T) {
	ps, err := ParseSource("p", []string{`
package p

type Item struct {
	Code string ` + "`bin:\"1\"`" + `
}

type ItemList []Item
type ItemPtrList []*Item

//gsbm:root
type Catalog struct {
	Direct   []Item      ` + "`bin:\"1\"`" + `
	Groups   ItemList    ` + "`bin:\"2\"`" + `
	Optional ItemPtrList ` + "`bin:\"3\"`" + `
}
`})
	if err != nil {
		t.Fatal(err)
	}
	res := Analyze(ps)
	if len(res.Issues) != 0 {
		t.Fatalf("issues: %s", FormatIssues(res.Issues))
	}
	var catalog *StructDecl
	for _, sd := range res.Schema.Structs {
		if sd.Type.Name == "Catalog" {
			catalog = sd
			break
		}
	}
	if catalog == nil {
		t.Fatal("Catalog struct not found")
	}
	if len(catalog.Fields) != 3 {
		t.Fatalf("expected 3 fields, got %d", len(catalog.Fields))
	}
	direct, groups, optional := catalog.Fields[0], catalog.Fields[1], catalog.Fields[2]
	if direct.AliasType != nil {
		t.Errorf("raw []Item field must have nil AliasType, got %+v", direct.AliasType)
	}
	if groups.AliasType == nil {
		t.Fatal("ItemList field must have AliasType set")
	}
	if groups.AliasType.Name != "ItemList" || groups.AliasType.Underlying == nil ||
		groups.AliasType.Underlying.Name != "Item" {
		t.Errorf("Groups.AliasType = %+v (Underlying=%+v); want Name=ItemList, Underlying.Name=Item",
			groups.AliasType, groups.AliasType.Underlying)
	}
	// fd.Type for the alias MUST be the underlying slice shape (not the
	// alias name) so a rename classifies as safe rather than type-changed.
	itemKey := refKey(TypeRef{PkgPath: "test/p", Name: "Item"})
	if groups.Type != "[]"+itemKey {
		t.Errorf("Groups.Type = %q; want rename-stable underlying-slice shape []%s", groups.Type, itemKey)
	}
	if optional.AliasType == nil || optional.AliasType.Name != "ItemPtrList" {
		t.Fatalf("Optional.AliasType = %+v; want Name=ItemPtrList", optional.AliasType)
	}
	if optional.AliasType.Underlying == nil ||
		optional.AliasType.Underlying.Name != "*"+itemKey {
		t.Errorf("Optional.AliasType.Underlying = %+v; want Name=*%s",
			optional.AliasType.Underlying, itemKey)
	}
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

// TestComputeSchemaHintReflectsAliasRename pins the canonical form's
// inclusion of AliasType: two schemas that differ only in the named
// alias identifier (same underlying slice) must produce different
// hints so the classifier's safe-rename emission still leaves an
// observable hash delta in the snapshot header.
func TestComputeSchemaHintReflectsAliasRename(t *testing.T) {
	itemRef := TypeRef{PkgPath: "p", Name: "Item"}
	alias1 := &TypeRef{PkgPath: "p", Name: "ItemList", Underlying: &itemRef}
	alias2 := &TypeRef{PkgPath: "p", Name: "Items", Underlying: &itemRef}
	a := makeSchema("T", []*FieldDecl{
		{Name: "Groups", Tag: 1, Type: "[]p.Item", Wire: WireLengthDelim, AliasType: alias1},
	})
	b := makeSchema("T", []*FieldDecl{
		{Name: "Groups", Tag: 1, Type: "[]p.Item", Wire: WireLengthDelim, AliasType: alias2},
	})
	if ComputeSchemaHint(a) == ComputeSchemaHint(b) {
		t.Fatalf("schemaHint should differ when only AliasType.Name changes")
	}
}
