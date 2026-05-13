package evolution

import (
	"path/filepath"
	"sort"
	"testing"

	"go.flaticols.dev/gsbm/storage/gsbm"
	"go.flaticols.dev/gsbm/tools/gsbmschema"

	addafter "go.flaticols.dev/gsbm/tools/gsbmcodegen/fixtures/evolution/addfield/after"
	addbefore "go.flaticols.dev/gsbm/tools/gsbmcodegen/fixtures/evolution/addfield/before"
)

// codes returns the sorted set of change codes in d.
func codes(d gsbmschema.Diff) []string {
	out := make([]string, 0, len(d.Changes))
	for _, c := range d.Changes {
		out = append(out, c.Code)
	}
	sort.Strings(out)
	return out
}

// findCode returns the first change with the given code, or nil.
func findCode(d gsbmschema.Diff, code string) *gsbmschema.Change {
	for i := range d.Changes {
		if d.Changes[i].Code == code {
			return &d.Changes[i]
		}
	}
	return nil
}

// loadPairForTest is the test-side wrapper that resolves the scenario
// directory under fixtureRoot and surfaces a t.Fatal on any error.
func loadPairForTest(t *testing.T, scenario string) (*gsbmschema.Schema, *gsbmschema.Schema) {
	t.Helper()
	root := fixtureRoot(t)
	prev, curr, err := LoadPair(filepath.Join(root, scenario, "before"), filepath.Join(root, scenario, "after"))
	if err != nil {
		t.Fatalf("LoadPair(%s): %v", scenario, err)
	}
	return prev, curr
}

// TestEvolutionAddField — adding a new tag to an existing root is the
// canonical safe change. Exactly one field/added entry; MaxSeverity
// stays safe; CI gate does not block.
func TestEvolutionAddField(t *testing.T) {
	prev, curr := loadPairForTest(t, "addfield")
	d := gsbmschema.Classify(prev, curr)

	if d.MaxSeverity != gsbmschema.SeveritySafe {
		t.Fatalf("MaxSeverity: got %s, want safe\n%s", d.MaxSeverity, gsbmschema.FormatDiff(d))
	}
	got := codes(d)
	want := []string{"field/added"}
	if !equalStrings(got, want) {
		t.Fatalf("codes: got %v, want %v\n%s", got, want, gsbmschema.FormatDiff(d))
	}
	c := findCode(d, "field/added")
	if c.Severity != gsbmschema.SeveritySafe {
		t.Fatalf("field/added severity: got %s, want safe", c.Severity)
	}
	if report := gsbmschema.CIDiff(prev, curr); report.GateBlocks {
		t.Fatalf("CI gate must not block on safe-add\n%s", gsbmschema.FormatDiff(report.Diff))
	}
}

// TestEvolutionCompatWriteReplace — landing compat_write on an existing
// tag plus adding its replacement is a two-change safe diff. Exercises
// the lifecycle state machine's active → compat_write transition
// alongside a fresh field/added.
func TestEvolutionCompatWriteReplace(t *testing.T) {
	prev, curr := loadPairForTest(t, "compatwrite")
	d := gsbmschema.Classify(prev, curr)

	if d.MaxSeverity != gsbmschema.SeveritySafe {
		t.Fatalf("MaxSeverity: got %s, want safe\n%s", d.MaxSeverity, gsbmschema.FormatDiff(d))
	}
	got := codes(d)
	want := []string{"field/added", "field/compat-write-added"}
	if !equalStrings(got, want) {
		t.Fatalf("codes: got %v, want %v\n%s", got, want, gsbmschema.FormatDiff(d))
	}
	for _, code := range want {
		if c := findCode(d, code); c == nil || c.Severity != gsbmschema.SeveritySafe {
			t.Fatalf("%s must be safe, got %v", code, c)
		}
	}
}

// TestEvolutionCompatWriteStopBakeGate — after→laterafter strips the
// compat_write annotation, leaving the field plain-deprecated. The
// classifier emits field/compat-write-removed at severity breaking;
// this transition is gated on calendar bake-time, which only the
// operator can attest to via DiffOptions.AllowStopCompatWrite. A
// //gsbm:allow-breaking source annotation is intentionally insufficient
// (see classifier.go:96-100) — but the production path also exercised
// in classifier_test.go covers that. Here we pin the fixture-driven
// flag-flip behavior end-to-end.
func TestEvolutionCompatWriteStopBakeGate(t *testing.T) {
	root := fixtureRoot(t)
	prev, curr, err := LoadPair(
		filepath.Join(root, "compatwrite", "after"),
		filepath.Join(root, "compatwrite", "laterafter"),
	)
	if err != nil {
		t.Fatalf("LoadPair: %v", err)
	}
	if c := findCode(gsbmschema.Classify(prev, curr), "field/compat-write-removed"); c == nil {
		t.Fatalf("expected field/compat-write-removed in diff")
	}
	blocked := gsbmschema.CIDiff(prev, curr)
	if !blocked.GateBlocks {
		t.Fatalf("without --allow-stop-compat-write, gate must block:\n%s", gsbmschema.FormatDiff(blocked.Diff))
	}
	allowed := gsbmschema.CIDiffWithOptions(prev, curr, gsbmschema.DiffOptions{AllowStopCompatWrite: true})
	if allowed.GateBlocks {
		t.Fatalf("with --allow-stop-compat-write, gate must not block:\n%s", gsbmschema.FormatDiff(allowed.Diff))
	}
	if allowed.Diff.MaxSeverity != gsbmschema.SeverityBreaking {
		t.Fatalf("severity stays breaking even when acknowledged, got %s", allowed.Diff.MaxSeverity)
	}
	c := findCode(allowed.Diff, "field/compat-write-removed")
	if c == nil || c.Acknowledged == "" {
		t.Fatalf("expected Acknowledged on field/compat-write-removed, got %v", c)
	}
}

// TestEvolutionWireTypeChange — switching a tag's Go type from string
// to int64 flips both the wire type (length-delim → varint) and the
// value type, so the classifier emits both field/wire-changed and
// field/type-changed at severity breaking. //gsbm:allow-breaking on
// the after struct flips GateBlocks from true to false.
func TestEvolutionWireTypeChange(t *testing.T) {
	prev, curr := loadPairForTest(t, "wirechange")
	d := gsbmschema.Classify(prev, curr)

	if d.MaxSeverity != gsbmschema.SeverityBreaking {
		t.Fatalf("MaxSeverity: got %s, want breaking\n%s", d.MaxSeverity, gsbmschema.FormatDiff(d))
	}
	got := codes(d)
	want := []string{"field/type-changed", "field/wire-changed"}
	if !equalStrings(got, want) {
		t.Fatalf("codes: got %v, want %v\n%s", got, want, gsbmschema.FormatDiff(d))
	}
	for _, code := range want {
		c := findCode(d, code)
		if c.Severity != gsbmschema.SeverityBreaking {
			t.Fatalf("%s severity: got %s, want breaking", code, c.Severity)
		}
	}
	assertBreakingGate(t, prev, curr)
}

// TestEvolutionTypeChange — same wire-type, different Go type
// (int32 → int64). Only field/type-changed fires; field/wire-changed
// must NOT, since both still encode as varint. That exclusivity is
// what distinguishes type-change from wire-change in the classifier.
func TestEvolutionTypeChange(t *testing.T) {
	prev, curr := loadPairForTest(t, "typechange")
	d := gsbmschema.Classify(prev, curr)

	if d.MaxSeverity != gsbmschema.SeverityBreaking {
		t.Fatalf("MaxSeverity: got %s, want breaking\n%s", d.MaxSeverity, gsbmschema.FormatDiff(d))
	}
	got := codes(d)
	want := []string{"field/type-changed"}
	if !equalStrings(got, want) {
		t.Fatalf("codes: got %v, want %v\n%s", got, want, gsbmschema.FormatDiff(d))
	}
	c := findCode(d, "field/type-changed")
	if c.Severity != gsbmschema.SeverityBreaking {
		t.Fatalf("field/type-changed severity: got %s, want breaking", c.Severity)
	}
	assertBreakingGate(t, prev, curr)
}

// TestEvolutionTagChange — moving a named field to a new tag fires
// field/tag-changed (breaking). The implementation also fires
// field/removed for the vacated tag (because the tag itself is gone
// from the curr fields-by-tag index); both codes appear at severity
// breaking. This dual-emission is the actual classifier contract,
// not just an implementation accident — pinning it here so a future
// dedup of one or the other surfaces as a deliberate behavior change.
func TestEvolutionTagChange(t *testing.T) {
	prev, curr := loadPairForTest(t, "tagchange")
	d := gsbmschema.Classify(prev, curr)

	if d.MaxSeverity != gsbmschema.SeverityBreaking {
		t.Fatalf("MaxSeverity: got %s, want breaking\n%s", d.MaxSeverity, gsbmschema.FormatDiff(d))
	}
	got := codes(d)
	want := []string{"field/removed", "field/tag-changed"}
	if !equalStrings(got, want) {
		t.Fatalf("codes: got %v, want %v\n%s", got, want, gsbmschema.FormatDiff(d))
	}
	for _, code := range want {
		c := findCode(d, code)
		if c == nil || c.Severity != gsbmschema.SeverityBreaking {
			t.Fatalf("%s severity: got %v, want breaking\n%s", code, c, gsbmschema.FormatDiff(d))
		}
	}
	assertBreakingGate(t, prev, curr)
}

// TestEvolutionFieldRemoved — outright removing an active field (not
// marking deprecated, not adding to //gsbm:reserved) is breaking under
// the append-only policy. The detail must steer the reviewer toward
// the lifecycle, not silently accept the deletion.
func TestEvolutionFieldRemoved(t *testing.T) {
	prev, curr := loadPairForTest(t, "removefield")
	d := gsbmschema.Classify(prev, curr)

	if d.MaxSeverity != gsbmschema.SeverityBreaking {
		t.Fatalf("MaxSeverity: got %s, want breaking\n%s", d.MaxSeverity, gsbmschema.FormatDiff(d))
	}
	got := codes(d)
	want := []string{"field/removed"}
	if !equalStrings(got, want) {
		t.Fatalf("codes: got %v, want %v\n%s", got, want, gsbmschema.FormatDiff(d))
	}
	c := findCode(d, "field/removed")
	if c.Severity != gsbmschema.SeverityBreaking {
		t.Fatalf("field/removed severity: got %s, want breaking", c.Severity)
	}
	assertBreakingGate(t, prev, curr)
}

// assertBreakingGate verifies the gate-blocking + ack-flip contract:
// CIDiff(prev, curr) blocks; setting AllowBreaking on every struct in
// curr (programmatic equivalent of //gsbm:allow-breaking applied to
// each) and re-running flips the gate to allowed. This is the same
// contract TestAllowBreakingOverride pins in classifier_test.go,
// applied per-fixture-pair. The schema passed in is mutated and must
// not be reused.
func assertBreakingGate(t *testing.T, prev, curr *gsbmschema.Schema) {
	t.Helper()
	if report := gsbmschema.CIDiff(prev, curr); !report.GateBlocks {
		t.Fatalf("expected CI gate to block without acknowledgement\n%s", gsbmschema.FormatDiff(report.Diff))
	}
	for _, sd := range curr.Structs {
		sd.AllowBreaking = "test acknowledgement"
	}
	report := gsbmschema.CIDiff(prev, curr)
	if report.GateBlocks {
		t.Fatalf("expected gate to allow once acknowledged\n%s", gsbmschema.FormatDiff(report.Diff))
	}
	if report.Diff.MaxSeverity != gsbmschema.SeverityBreaking {
		t.Fatalf("severity must remain breaking even when acknowledged, got %s", report.Diff.MaxSeverity)
	}
}

// TestEvolutionAddFieldRoundTripForward — a blob produced by the
// before-package encoder is decoded by the after-package decoder. The
// new tag's field reads as its zero value and is not marked present;
// every old tag round-trips. This is the safe-add property at the
// wire level, complementing the classifier-level assertion in
// TestEvolutionAddField.
func TestEvolutionAddFieldRoundTripForward(t *testing.T) {
	src := addbefore.Shipment{Carrier: "DHL", TrackingID: "1Z999"}
	w := gsbm.NewWriter(nil)
	if err := src.MarshalGSBM(w); err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var dst addafter.Shipment
	if err := dst.UnmarshalGSBM(gsbm.NewReader(w.Bytes())); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if dst.Carrier != src.Carrier || dst.TrackingID != src.TrackingID {
		t.Fatalf("old tags lost in round-trip: got %#v", dst)
	}
	if dst.Weight != 0 {
		t.Fatalf("Weight: got %d, want 0", dst.Weight)
	}
	// addafter.Shipment is a default-mode type; FieldPresent returns
	// false post-decode regardless of which tags appeared on the wire.
	// The schema evolution proof above (old fields round-trip, new tag
	// reads zero) is the load-bearing assertion.
	for _, tag := range []uint32{1, 2, 3} {
		if dst.FieldPresent(tag) {
			t.Fatalf("FieldPresent(%d) = true; default-mode receiver must report all tags absent post-decode", tag)
		}
	}
}

// TestEvolutionAddFieldRoundTripReverse — the forward-compatibility
// property: an after-encoded blob (carrying the new tag 3) must decode
// cleanly into the before struct. The unknown tag is routed through
// SkipField in the generated default branch, so old readers stay
// functional even after the rollout.
func TestEvolutionAddFieldRoundTripReverse(t *testing.T) {
	src := addafter.Shipment{Carrier: "DHL", TrackingID: "1Z999", Weight: 42}
	w := gsbm.NewWriter(nil)
	if err := src.MarshalGSBM(w); err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var dst addbefore.Shipment
	if err := dst.UnmarshalGSBM(gsbm.NewReader(w.Bytes())); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if dst.Carrier != src.Carrier || dst.TrackingID != src.TrackingID {
		t.Fatalf("old tags lost in reverse round-trip: got %#v", dst)
	}
	for _, tag := range []uint32{1, 2} {
		if dst.FieldPresent(tag) {
			t.Fatalf("FieldPresent(%d) = true; default-mode receiver must report all tags absent post-decode", tag)
		}
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
