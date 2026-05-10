package graph

import (
	"errors"
	"reflect"
	"testing"

	"go.flaticols.dev/gsbm/storage/gsbm"
)

func strp(s string) *string { return &s }
func i64p(v int64) *int64   { return &v }

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

// TestCatalogRoundTrip exercises the supported shapes end-to-end:
// two-deep named composition (Catalog → Section → Item), the
// map<string, named-struct> shape (Tags), the optional-primitive
// presence path on Item.Note and Tag.Weight (nil / zero-elided /
// non-zero), and the high-tag (2^29 - 1) Tail field. Plan Task 2's
// "[]*Section / map[string]*Tag / map[Code]*Tag" wording was scoped
// down in Task 1 (see types.go header) — those shapes are unsupported
// by the validator/codegen today, so this test pins the supported
// pivot: []Section, map[string]Tag.
func TestCatalogRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		in   Catalog
	}{
		{
			name: "fully populated",
			in: Catalog{
				ID: "cat-001",
				Sections: []Section{
					{
						Name: "north",
						Items: []Item{
							{SKU: "abc", Note: strp("hot")},
							{SKU: "def", Note: nil},
							{SKU: "ghi", Note: strp("")}, // zero-elide path
						},
					},
					{
						Name:  "south",
						Items: nil,
					},
				},
				Tags: map[string]Tag{
					"primary": {Slug: "p", Weight: i64p(7)},
					"backup":  {Slug: "b", Weight: nil},
					"zero":    {Slug: "z", Weight: i64p(0)}, // zero-elide path on int64 pointer
				},
				Tail: EdgeMarker{Marker: true},
			},
		},
		{
			name: "nil-everywhere",
			in:   Catalog{},
		},
		{
			name: "sections-only",
			in: Catalog{
				ID: "cat-sections",
				Sections: []Section{
					{Name: "only", Items: []Item{{SKU: "x"}}},
				},
			},
		},
		{
			name: "tags-only",
			in: Catalog{
				ID: "cat-tags",
				Tags: map[string]Tag{
					"k": {Slug: "v"},
				},
			},
		},
		{
			name: "high-tag-only",
			in:   Catalog{Tail: EdgeMarker{Marker: true}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var w gsbm.Writer
			if err := tc.in.MarshalGSBM(&w); err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var got Catalog
			if err := got.UnmarshalGSBM(gsbm.NewReader(w.Bytes())); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			want := normalizeCatalog(tc.in)
			if !reflect.DeepEqual(want, got) {
				t.Fatalf("round-trip mismatch\n want: %#v\n  got: %#v", want, got)
			}
		})
	}
}

// normalizeCatalog mirrors decoder behavior for empty collections so
// DeepEqual matches: the encoder writes len=0 and the decoder leaves
// the destination slice/map at its nil zero value (the "if n > 0"
// guard in the generated code suppresses allocation for zero counts).
func normalizeCatalog(c Catalog) Catalog {
	if len(c.Sections) == 0 {
		c.Sections = nil
	} else {
		for i := range c.Sections {
			if len(c.Sections[i].Items) == 0 {
				c.Sections[i].Items = nil
			}
		}
	}
	if len(c.Tags) == 0 {
		c.Tags = nil
	}
	return c
}

// TestCatalogPresenceBitmap pins the FieldPresent contract on Catalog:
//   - Tags ≤ MaxTrackedTag the encoder wrote are reported present; tags
//     it didn't write are reported absent (distinguishing "missing" from
//     "present with the zero value" for tag 1, which holds an empty
//     string after decode).
//   - The high tag (Tail = 2^29 - 1) is intentionally NOT tracked by
//     the sidecar bitmap: the codegen emitted MarkPresent for it, but
//     gsbm.MarkPresent silently ignores tags > MaxTrackedTag (1024)
//     (see storage/gsbm/presence_track.go:79). The fixture's types.go
//     and the plan both note this as documented behavior, not a bug.
//     We assert FieldPresent(536870911) returns false to lock in that
//     contract; future code that decides to track high tags must
//     update this expectation deliberately.
func TestCatalogPresenceBitmap(t *testing.T) {
	// Encode a fully populated Catalog and confirm tracked tags are
	// flagged present after decode.
	in := Catalog{
		ID:       "rt",
		Sections: []Section{{Name: "s"}},
		Tags:     map[string]Tag{"k": {Slug: "v"}},
		Tail:     EdgeMarker{Marker: true},
	}
	var w gsbm.Writer
	if err := in.MarshalGSBM(&w); err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got Catalog
	if err := got.UnmarshalGSBM(gsbm.NewReader(w.Bytes())); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, tag := range []uint32{1, 2, 3} {
		if !got.FieldPresent(tag) {
			t.Errorf("FieldPresent(%d) = false, want true (encoder wrote it)", tag)
		}
	}
	if got.FieldPresent(536870911) {
		t.Errorf("FieldPresent(536870911) = true, want false (tag exceeds MaxTrackedTag=%d, sidecar silently drops)", gsbm.MaxTrackedTag)
	}
	if got.FieldPresent(0) {
		t.Error("FieldPresent(0) = true, want false (tag 0 is reserved)")
	}
	if got.FieldPresent(4) {
		t.Error("FieldPresent(4) = true, want false (no tag 4 declared)")
	}

	// Hand-craft a partial blob: only tag 1 (ID) is written. Tags 2,
	// 3, and 536870911 must report absent — distinguishing the
	// "missing" case from "present but zero" since tag 1 carries the
	// empty string.
	var w2 gsbm.Writer
	w2.WriteTag(1, gsbm.WireLengthDelim)
	w2.WriteString("")
	if w2.Err() != nil {
		t.Fatal(w2.Err())
	}
	var partial Catalog
	if err := partial.UnmarshalGSBM(gsbm.NewReader(w2.Bytes())); err != nil {
		t.Fatalf("unmarshal partial: %v", err)
	}
	if !partial.FieldPresent(1) {
		t.Error("FieldPresent(1) = false on partial blob; expected present-zero (empty string written on the wire)")
	}
	for _, tag := range []uint32{2, 3} {
		if partial.FieldPresent(tag) {
			t.Errorf("FieldPresent(%d) = true on partial blob; expected absent (encoder did not write it)", tag)
		}
	}
}

// TestCatalogDuplicateTagLastWins exercises spec §3.3: when a struct
// blob contains the same tag twice, the decoder takes the last value.
// The generated decoder gets this for free — the per-tag switch case
// simply overwrites the destination field on each occurrence — so the
// test is here to lock the behavior against future codegen changes
// that might (e.g.) reject duplicates as a hardening measure.
func TestCatalogDuplicateTagLastWins(t *testing.T) {
	var w gsbm.Writer
	w.WriteTag(1, gsbm.WireLengthDelim)
	w.WriteString("first")
	w.WriteTag(1, gsbm.WireLengthDelim)
	w.WriteString("second")
	if w.Err() != nil {
		t.Fatal(w.Err())
	}
	var got Catalog
	if err := got.UnmarshalGSBM(gsbm.NewReader(w.Bytes())); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.ID != "second" {
		t.Errorf("Catalog.ID = %q, want %q (spec §3.3: duplicate tags last-wins)", got.ID, "second")
	}
}

// TestCatalogDuplicateMapKeyLastWins exercises spec §3.3 for map
// payloads: a hand-crafted Tags field declares two pairs with the
// same key but different values. The decode loop assigns to
// v.Tags[k] on each iteration, so the second value overwrites the
// first. This pins the "last entry wins" rule for map encoding.
func TestCatalogDuplicateMapKeyLastWins(t *testing.T) {
	var w gsbm.Writer
	w.WriteTag(3, gsbm.WireLengthDelim) // Tags
	outer := w.BeginLengthDelim()
	w.WriteUvarint(2) // pair count
	// pair 1: ("dup", Tag{Slug:"first"})
	w.WriteString("dup")
	{
		inner := w.BeginLengthDelim()
		// Tag.Slug
		w.WriteTag(1, gsbm.WireLengthDelim)
		w.WriteString("first")
		// Tag.Weight (nil presence)
		w.WriteTag(2, gsbm.WireLengthDelim)
		m := w.BeginLengthDelim()
		w.WritePresenceNil()
		w.EndLengthDelim(m)
		w.EndLengthDelim(inner)
	}
	// pair 2: ("dup", Tag{Slug:"second"})
	w.WriteString("dup")
	{
		inner := w.BeginLengthDelim()
		w.WriteTag(1, gsbm.WireLengthDelim)
		w.WriteString("second")
		w.WriteTag(2, gsbm.WireLengthDelim)
		m := w.BeginLengthDelim()
		w.WritePresenceNil()
		w.EndLengthDelim(m)
		w.EndLengthDelim(inner)
	}
	w.EndLengthDelim(outer)
	if w.Err() != nil {
		t.Fatal(w.Err())
	}
	var got Catalog
	if err := got.UnmarshalGSBM(gsbm.NewReader(w.Bytes())); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.Tags) != 1 {
		t.Fatalf("Tags has %d entries, want 1 (duplicate key should collapse): %#v", len(got.Tags), got.Tags)
	}
	tag, ok := got.Tags["dup"]
	if !ok {
		t.Fatalf("missing key %q in Tags: %#v", "dup", got.Tags)
	}
	if tag.Slug != "second" {
		t.Errorf("Tags[%q].Slug = %q, want %q (spec §3.3: duplicate map keys last-wins)", "dup", tag.Slug, "second")
	}
}

// TestCatalogLengthBoundedRegionOverflow exercises the spec §3 rule
// that a LENGTH_DELIM field whose declared length exceeds bytes
// remaining in the enclosing region MUST be rejected. Reader.
// BeginLengthDelim enforces this at storage/gsbm/reader.go:280-288
// by surfacing ErrTruncated; this test pins the rule end-to-end via
// generated decoder code.
func TestCatalogLengthBoundedRegionOverflow(t *testing.T) {
	var w gsbm.Writer
	w.WriteTag(2, gsbm.WireLengthDelim) // Sections
	// Declare a body length of 1024 bytes but write nothing.
	w.WriteUvarint(1024)
	if w.Err() != nil {
		t.Fatal(w.Err())
	}
	var got Catalog
	err := got.UnmarshalGSBM(gsbm.NewReader(w.Bytes()))
	if !errors.Is(err, gsbm.ErrTruncated) {
		t.Fatalf("decode: want ErrTruncated, got %v", err)
	}
}

// TestCatalogHighTagRoundTrip pins the upper-boundary varint behavior
// for the field-key encoding. Tag 2^29 - 1 = 536870911 is the largest
// tag the spec admits (one more would overflow per spec §3.1). The
// field key (tag<<3 | wt) is 0xFFFFFFFA, which encodes as a 5-byte
// varint — the wire-format limit for a tag varint. The test
// round-trips a Catalog with Tail.Marker=true and asserts both the
// payload survives and the encoded field key occupies exactly 5 bytes
// at the expected offset, locking in the encoding shape.
func TestCatalogHighTagRoundTrip(t *testing.T) {
	in := Catalog{Tail: EdgeMarker{Marker: true}}
	var w gsbm.Writer
	if err := in.MarshalGSBM(&w); err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got Catalog
	if err := got.UnmarshalGSBM(gsbm.NewReader(w.Bytes())); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !got.Tail.Marker {
		t.Errorf("Tail.Marker = false, want true after round-trip of tag 2^29-1")
	}

	// Locate and verify the high-tag field key directly. The encoded
	// blob is: <tag1 key + body> <tag2 key + body> <tag3 key + body>
	// <tag 2^29-1 key + body>. The first three keys are 1-byte
	// varints (tags ≤ 15 with wt=2: key ≤ 0x7E), so we scan past
	// length-prefixed bodies until we find a 5-byte varint.
	buf := w.Bytes()
	pos := 0
	for range 3 {
		// 1-byte field key for tags ≤ 15.
		if buf[pos]&0x80 != 0 {
			t.Fatalf("expected 1-byte field key at pos %d, got continuation byte 0x%02x", pos, buf[pos])
		}
		pos++
		// Read varint length, skip body.
		l, n := readUvarintAt(buf, pos)
		pos += n + int(l)
	}
	// pos now points at the 5-byte varint for tag 536870911.
	keyStart := pos
	for i := range 5 {
		if pos >= len(buf) {
			t.Fatalf("ran off end while reading high-tag field key starting at %d", keyStart)
		}
		more := buf[pos]&0x80 != 0
		pos++
		if i < 4 && !more {
			t.Fatalf("high-tag field key shorter than 5 bytes (terminated at byte %d): % x", i+1, buf[keyStart:pos])
		}
		if i == 4 && more {
			t.Fatalf("high-tag field key longer than 5 bytes (continuation set on byte 5): % x", buf[keyStart:pos])
		}
	}
	// Sanity: decode the key and assert it's the expected (tag, wt) pair.
	key, _ := readUvarintAt(buf, keyStart)
	wantTag := uint64(536870911)
	wantWT := uint64(gsbm.WireLengthDelim)
	if key>>3 != wantTag || key&0x7 != wantWT {
		t.Fatalf("high-tag field key decodes to (tag=%d, wt=%d), want (tag=%d, wt=%d)", key>>3, key&0x7, wantTag, wantWT)
	}
}

// readUvarintAt is a tiny, allocation-free varint reader used by the
// high-tag round-trip test to walk the encoded blob without depending
// on Reader's mutable state.
func readUvarintAt(buf []byte, pos int) (uint64, int) {
	var v uint64
	var shift uint
	n := 0
	for pos+n < len(buf) {
		b := buf[pos+n]
		n++
		v |= uint64(b&0x7F) << shift
		if b&0x80 == 0 {
			return v, n
		}
		shift += 7
	}
	return v, n
}
