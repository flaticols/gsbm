package nestedcomp

import (
	"bytes"
	"reflect"
	"testing"

	"go.flaticols.dev/gsbm/storage/gsbm"
)

// TestIndexRoundTrip exercises every nested-composite shape together so
// each level's BeginLengthDelim/EndLengthDelim framing has to line up
// for the whole blob to decode. A mismatch at any depth surfaces as a
// truncation error or a DeepEqual divergence at the leaf.
func TestIndexRoundTrip(t *testing.T) {
	in := Index{
		IDsByGroup: map[string][]string{
			"a": {"x", "y", "z"},
			"c": {"only"},
		},
		LabelsByGroup: map[string]map[string]string{
			"alpha": {"role": "admin", "tier": "gold"},
			"beta":  {"role": "guest"},
		},
		MetadataVariants: []map[string]string{
			{"k1": "v1", "k2": "v2"},
			{"only": "one"},
		},
		Deep: map[string][]map[string]int64{
			"north": {{"a": 1, "b": 2}, {"c": 3}},
			"south": {{"x": -1}},
		},
	}
	w := gsbm.NewWriter(nil)
	if err := in.MarshalGSBM(w); err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got Index
	if err := got.UnmarshalGSBM(gsbm.NewReader(w.Bytes())); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(in, got) {
		t.Fatalf("round-trip mismatch\n want: %#v\n  got: %#v", in, got)
	}
}

// TestMapOfSliceRoundTrip isolates the map[K][]V shape so a failure
// points at the map→slice recursion path without other fields masking
// the diff.
func TestMapOfSliceRoundTrip(t *testing.T) {
	in := Index{
		IDsByGroup: map[string][]string{
			"x": {"a", "b"},
			"y": {"c"},
		},
	}
	if !roundTripEqual(t, in) {
		t.Fatal("map[string][]string round-trip mismatch")
	}
}

// TestMapOfMapRoundTrip isolates the map[K]map[K2]V shape so failures
// point at the map→map recursion path.
func TestMapOfMapRoundTrip(t *testing.T) {
	in := Index{
		LabelsByGroup: map[string]map[string]string{
			"g1": {"role": "admin"},
			"g2": {"role": "guest", "tier": "silver"},
		},
	}
	if !roundTripEqual(t, in) {
		t.Fatal("map[string]map[string]string round-trip mismatch")
	}
}

// TestSliceOfMapRoundTrip isolates the []map[K]V shape so failures
// point at the slice→map recursion path. This is the case that
// exercises the slice element being a map decoded into expr[i].
func TestSliceOfMapRoundTrip(t *testing.T) {
	in := Index{
		MetadataVariants: []map[string]string{
			{"a": "1", "b": "2"},
			{"only": "one"},
		},
	}
	if !roundTripEqual(t, in) {
		t.Fatal("[]map[string]string round-trip mismatch")
	}
}

// TestThreeDeepRoundTrip exercises MaxNestingDepth=3 (map→slice→map).
// This is the deepest legal shape; one more level would be rejected by
// the validator with a type/nesting-too-deep issue.
func TestThreeDeepRoundTrip(t *testing.T) {
	in := Index{
		Deep: map[string][]map[string]int64{
			"k1": {{"a": 1}, {"b": 2, "c": 3}},
			"k2": {{"x": -42}},
		},
	}
	if !roundTripEqual(t, in) {
		t.Fatal("map[string][]map[string]int64 round-trip mismatch")
	}
}

// TestSkipNestedUnknownField is the spec §3.2 guarantee at this issue's
// core: a tag the decoder doesn't know about must be skippable using
// only the outer LENGTH_DELIM length, no matter how deeply nested the
// payload is. We hand-craft a blob whose unknown tag carries a
// map[K]map[K2]V payload and confirm the decoder resyncs cleanly to
// the next known tag.
func TestSkipNestedUnknownField(t *testing.T) {
	// Build a blob that starts with a real Index (tag 1) and follows
	// with an unknown tag (1000) carrying a nested map[K]map[K2]V
	// payload, then continues with tag 2 (real LabelsByGroup). A
	// correctly-implemented SkipField should jump straight from end of
	// tag 1 over tag 1000 to tag 2.
	in := Index{
		IDsByGroup:    map[string][]string{"first": {"a"}},
		LabelsByGroup: map[string]map[string]string{"last": {"k": "v"}},
	}

	w := gsbm.NewWriter(nil)
	// Manually emit tag 1 the way MarshalGSBM would (we just need bytes
	// that match the on-wire layout).
	w.WriteTag(1, gsbm.WireLengthDelim)
	{
		m := w.BeginLengthDelim()
		w.WriteUvarint(uint64(len(in.IDsByGroup)))
		w.WriteString("first")
		// inner slice for "first"
		m2 := w.BeginLengthDelim()
		w.WriteUvarint(1)
		w.WriteString("a")
		w.EndLengthDelim(m2)
		w.EndLengthDelim(m)
	}

	// Unknown tag 1000 carrying a nested map-of-map payload. The
	// decoder must skip purely via the outer LENGTH_DELIM length,
	// never inspecting the nested structure.
	w.WriteTag(1000, gsbm.WireLengthDelim)
	{
		m := w.BeginLengthDelim()
		w.WriteUvarint(2) // outer count
		// entry 1
		w.WriteString("aaa")
		m2 := w.BeginLengthDelim()
		w.WriteUvarint(2)
		w.WriteString("k1")
		w.WriteString("v1")
		w.WriteString("k2")
		w.WriteString("v2")
		w.EndLengthDelim(m2)
		// entry 2
		w.WriteString("bbb")
		m3 := w.BeginLengthDelim()
		w.WriteUvarint(0)
		w.EndLengthDelim(m3)
		w.EndLengthDelim(m)
	}

	// Tag 2 LabelsByGroup, as the codegen would emit it.
	w.WriteTag(2, gsbm.WireLengthDelim)
	{
		m := w.BeginLengthDelim()
		w.WriteUvarint(uint64(len(in.LabelsByGroup)))
		w.WriteString("last")
		m2 := w.BeginLengthDelim()
		w.WriteUvarint(1)
		w.WriteString("k")
		w.WriteString("v")
		w.EndLengthDelim(m2)
		w.EndLengthDelim(m)
	}
	if w.Err() != nil {
		t.Fatalf("builder: %v", w.Err())
	}

	got := &Index{}
	if err := got.UnmarshalGSBM(gsbm.NewReader(w.Bytes())); err != nil {
		t.Fatalf("unmarshal blob with unknown nested tag: %v", err)
	}
	want := &Index{
		IDsByGroup:    map[string][]string{"first": {"a"}},
		LabelsByGroup: map[string]map[string]string{"last": {"k": "v"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("skip-then-resync mismatch\n want: %#v\n  got: %#v", want, got)
	}
	// Index has no //gsbm:track-presence marker; default-mode receivers
	// must report all tags absent after decode (the local bitmap dies
	// with the UnmarshalGSBM call). Round-trip fidelity is asserted via
	// the DeepEqual above, not via the sidecar.
	for _, tag := range []uint32{1, 2, 3, 1000} {
		if got.FieldPresent(tag) {
			t.Errorf("FieldPresent(%d) = true; default-mode Index must report all tags absent", tag)
		}
	}
}

// TestEmptyRoundTrip pins nil-in / nil-out: an empty Index encodes a
// 4-tag blob (each field's empty length-delim body) and decodes back to
// all-nil maps/slices, mirroring the namedkey fixture's contract.
func TestEmptyRoundTrip(t *testing.T) {
	in := Index{}
	w := gsbm.NewWriter(nil)
	if err := in.MarshalGSBM(w); err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got Index
	if err := got.UnmarshalGSBM(gsbm.NewReader(w.Bytes())); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.IDsByGroup != nil || got.LabelsByGroup != nil || got.MetadataVariants != nil || got.Deep != nil {
		t.Fatalf("expected nil composites after empty round-trip, got %#v", got)
	}
}

// TestDecodeIntoReusesNestedComposite pins the bug where re-decoding into
// a receiver with retained slice capacity merged keys from the previous
// payload's inner map into the new payload. The slice []map[K]V truncates
// to [:0] on Reset but the backing array's old maps survive; without a
// clear in the decoder before re-populating, the inner map[K]V picks up
// stale keys via the cap-reuse branch.
func TestDecodeIntoReusesNestedComposite(t *testing.T) {
	first := &Index{
		MetadataVariants: []map[string]string{
			{"old1": "a"},
			{"old2": "b"},
		},
	}
	w := gsbm.NewWriter(nil)
	if err := first.MarshalGSBM(w); err != nil {
		t.Fatalf("marshal first: %v", err)
	}

	second := &Index{
		MetadataVariants: []map[string]string{
			{"new": "data"},
		},
	}
	w2 := gsbm.NewWriter(nil)
	if err := second.MarshalGSBM(w2); err != nil {
		t.Fatalf("marshal second: %v", err)
	}

	got := &Index{}
	if err := got.UnmarshalGSBM(gsbm.NewReader(w.Bytes())); err != nil {
		t.Fatalf("first unmarshal: %v", err)
	}
	got.Reset()
	if err := got.UnmarshalGSBM(gsbm.NewReader(w2.Bytes())); err != nil {
		t.Fatalf("second unmarshal: %v", err)
	}
	if !reflect.DeepEqual(got.MetadataVariants, second.MetadataVariants) {
		t.Fatalf("stale-state leak on cap-reuse\n want: %#v\n  got: %#v", second.MetadataVariants, got.MetadataVariants)
	}
}

// roundTripEqual marshals in, unmarshals into a fresh Index, and
// reports whether the result DeepEquals in. Wire bytes are also
// validated to be self-stable: re-encoding the decoded value must
// produce identical bytes (deterministic encoding).
func roundTripEqual(t *testing.T, in Index) bool {
	t.Helper()
	w := gsbm.NewWriter(nil)
	if err := in.MarshalGSBM(w); err != nil {
		t.Fatalf("marshal: %v", err)
	}
	first := append([]byte(nil), w.Bytes()...)
	var got Index
	if err := got.UnmarshalGSBM(gsbm.NewReader(first)); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(in, got) {
		t.Logf("decoded mismatch\n want: %#v\n  got: %#v", in, got)
		return false
	}
	// Re-encode and confirm wire stability.
	w2 := gsbm.NewWriter(nil)
	if err := got.MarshalGSBM(w2); err != nil {
		t.Fatalf("re-marshal: %v", err)
	}
	if !bytes.Equal(first, w2.Bytes()) {
		t.Logf("re-encoded bytes differ\n  first: %x\n second: %x", first, w2.Bytes())
		return false
	}
	return true
}
