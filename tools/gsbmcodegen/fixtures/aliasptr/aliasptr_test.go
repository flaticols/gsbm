package aliasptr

import (
	"bytes"
	"reflect"
	"testing"

	"go.flaticols.dev/gsbm/storage/gsbm"
	"go.flaticols.dev/gsbm/storage/gsbmarena"
)

func ptr[T any](v T) *T { return &v }

// TestSliceOfPointerRoundTrip exercises []*T with all-present pointers
// across two element types (Item with optional Note, OptionalNote with a
// plain string). DeepEqual confirms nothing leaks across the wire.
func TestSliceOfPointerRoundTrip(t *testing.T) {
	in := Batch{
		Items: []*Item{
			{SKU: "a-1", Note: ptr("first")},
			{SKU: "b-2"},
			{SKU: "c-3", Note: ptr("third")},
		},
		Optional: []*OptionalNote{
			{Value: "one"},
			{Value: "two"},
		},
	}
	got := roundTrip(t, &in)
	if !reflect.DeepEqual(in, *got) {
		t.Fatalf("mismatch\n want: %#v\n  got: %#v", in, *got)
	}
}

// TestSliceOfPointerNilInMiddle pins the spec §5.1 nil-element guarantee:
// `Items[1]` is nil on the wire and decodes back as nil at the same index.
// Without per-element length-delim envelopes the nil would either shift
// indices or be silently dropped.
func TestSliceOfPointerNilInMiddle(t *testing.T) {
	in := Batch{
		Items: []*Item{
			{SKU: "a", Note: ptr("first")},
			nil,
			{SKU: "c"},
		},
	}
	got := roundTrip(t, &in)
	if len(got.Items) != 3 {
		t.Fatalf("len(Items): want 3, got %d", len(got.Items))
	}
	if got.Items[1] != nil {
		t.Fatalf("Items[1]: want nil, got %#v", got.Items[1])
	}
	if !reflect.DeepEqual(in, *got) {
		t.Fatalf("mismatch\n want: %#v\n  got: %#v", in, *got)
	}
}

// TestEmptySliceRoundTrip — an empty (but non-nil) slice on encode comes
// back as a nil slice on decode. The wire records length 0; the decoder
// short-circuits the MakeSlice/reuse block and leaves the field at its
// post-Reset state (nil for a freshly-zeroed receiver). This pins that
// contract rather than claiming an empty-non-nil round-trip.
func TestEmptySliceRoundTrip(t *testing.T) {
	in := Batch{
		Items:          []*Item{},
		Optional:       []*OptionalNote{},
		Groups:         ItemList{},
		OptionalGroups: ItemPtrList{},
	}
	got := roundTrip(t, &in)
	if got.Items != nil || got.Optional != nil || got.Groups != nil || got.OptionalGroups != nil {
		t.Fatalf("expected nil-out for empty-in, got %#v", got)
	}
}

// TestNilSliceRoundTrip pins the nil-in / nil-out contract symmetrically.
func TestNilSliceRoundTrip(t *testing.T) {
	in := Batch{}
	got := roundTrip(t, &in)
	if got.Items != nil || got.Optional != nil || got.Groups != nil || got.OptionalGroups != nil {
		t.Fatalf("expected nil slices after nil-in round-trip, got %#v", got)
	}
}

// TestNamedAliasRoundTrip covers the value-element named alias path:
// `Groups ItemList` round-trips through DeepEqual.
func TestNamedAliasRoundTrip(t *testing.T) {
	in := Batch{
		Groups: ItemList{
			{SKU: "g-1", Note: ptr("a")},
			{SKU: "g-2"},
		},
	}
	got := roundTrip(t, &in)
	if !reflect.DeepEqual(in, *got) {
		t.Fatalf("mismatch\n want: %#v\n  got: %#v", in, *got)
	}
}

// TestNamedAliasOverPointerRoundTrip covers `OptionalGroups ItemPtrList`
// with a nil interleaved: alias-over-pointer must preserve mid-slice nil
// exactly like the unwrapped `[]*Item` form.
func TestNamedAliasOverPointerRoundTrip(t *testing.T) {
	in := Batch{
		OptionalGroups: ItemPtrList{
			{SKU: "p-1"},
			nil,
			{SKU: "p-3", Note: ptr("note")},
		},
	}
	got := roundTrip(t, &in)
	if len(got.OptionalGroups) != 3 || got.OptionalGroups[1] != nil {
		t.Fatalf("OptionalGroups did not preserve mid-slice nil: %#v", got.OptionalGroups)
	}
	if !reflect.DeepEqual(in, *got) {
		t.Fatalf("mismatch\n want: %#v\n  got: %#v", in, *got)
	}
}

// TestNamedAliasWireMatchesUnderlying is the load-bearing wire-stability
// claim: encoding `Groups ItemList` produces byte-identical bytes for
// tag 3 to a hand-rolled `[]Item` encoded over the same data. Without
// this, callers cannot rename `[]Item` to `type ItemList []Item` without
// invalidating on-disk blobs.
func TestNamedAliasWireMatchesUnderlying(t *testing.T) {
	items := ItemList{
		{SKU: "g-1", Note: ptr("a")},
		{SKU: "g-2"},
	}
	in := Batch{Groups: items}
	w := gsbm.NewWriter(nil)
	if err := in.MarshalGSBM(w); err != nil {
		t.Fatalf("marshal: %v", err)
	}

	// Hand-build the expected wire bytes: a Batch with only tag 3
	// (Groups) populated, encoded as the underlying []Item slice form
	// (length-prefixed loop of length-delim-wrapped struct bodies). All
	// other tags are emitted as empty slices to match the codegen
	// (encoders always emit every non-deprecated field, even when nil).
	want := gsbm.NewWriter(nil)
	emitEmptySlice(want, 1)
	emitEmptySlice(want, 2)
	want.WriteTag(3, gsbm.WireLengthDelim)
	m := want.BeginLengthDelim()
	want.WriteUvarint(uint64(len(items)))
	for i := range items {
		inner := want.BeginLengthDelim()
		if err := items[i].MarshalGSBM(want); err != nil {
			t.Fatalf("inner marshal: %v", err)
		}
		want.EndLengthDelim(inner)
	}
	want.EndLengthDelim(m)
	emitEmptySlice(want, 4)
	if want.Err() != nil {
		t.Fatalf("want builder: %v", want.Err())
	}

	if !bytes.Equal(w.Bytes(), want.Bytes()) {
		t.Fatalf("named-alias wire bytes differ from underlying baseline\n got: %x\nwant: %x", w.Bytes(), want.Bytes())
	}
}

// TestNamedAliasOverPointerWireMatchesUnderlying is the pointer-element
// counterpart to TestNamedAliasWireMatchesUnderlying: `ItemPtrList`
// (alias over `[]*Item`) must produce byte-identical wire bytes to a
// hand-rolled `[]*Item` encoded over the same data.
func TestNamedAliasOverPointerWireMatchesUnderlying(t *testing.T) {
	items := ItemPtrList{
		{SKU: "p-1"},
		nil,
		{SKU: "p-3", Note: ptr("n")},
	}
	in := Batch{OptionalGroups: items}
	w := gsbm.NewWriter(nil)
	if err := in.MarshalGSBM(w); err != nil {
		t.Fatalf("marshal: %v", err)
	}

	want := gsbm.NewWriter(nil)
	emitEmptySlice(want, 1)
	emitEmptySlice(want, 2)
	emitEmptySlice(want, 3)
	want.WriteTag(4, gsbm.WireLengthDelim)
	m := want.BeginLengthDelim()
	want.WriteUvarint(uint64(len(items)))
	for i := range items {
		inner := want.BeginLengthDelim()
		if items[i] == nil {
			want.WritePresenceNil()
		} else {
			want.WritePresenceNonZero()
			if err := items[i].MarshalGSBM(want); err != nil {
				t.Fatalf("inner marshal: %v", err)
			}
		}
		want.EndLengthDelim(inner)
	}
	want.EndLengthDelim(m)
	if want.Err() != nil {
		t.Fatalf("want builder: %v", want.Err())
	}

	if !bytes.Equal(w.Bytes(), want.Bytes()) {
		t.Fatalf("named-alias-over-pointer wire bytes differ from underlying baseline\n got: %x\nwant: %x", w.Bytes(), want.Bytes())
	}
}

// TestArenaRoundTrip exercises the arena Decode/Detach helpers against
// the full mixed Batch (pointer slices + alias slices). Detach lifts to
// heap so the release-after assertion can run safely.
func TestArenaRoundTrip(t *testing.T) {
	in := Batch{
		Items: []*Item{
			{SKU: "a", Note: ptr("first")},
			nil,
			{SKU: "c"},
		},
		Optional: []*OptionalNote{{Value: "x"}},
		Groups:   ItemList{{SKU: "g"}},
		OptionalGroups: ItemPtrList{
			{SKU: "p"},
			nil,
		},
	}
	w := gsbm.NewWriter(nil)
	w.WriteHeader(0, 1)
	if err := in.MarshalGSBM(w); err != nil {
		t.Fatalf("marshal: %v", err)
	}
	blob := w.Bytes()

	a := gsbmarena.NewArena()
	got, err := DecodeBatch(blob, a)
	if err != nil {
		t.Fatalf("arena decode: %v", err)
	}
	if !reflect.DeepEqual(in, *got) {
		t.Fatalf("arena decode mismatch\n want: %#v\n  got: %#v", in, *got)
	}
	detached, err := DetachBatch(got)
	if err != nil {
		t.Fatalf("detach: %v", err)
	}
	a.Release()
	if !reflect.DeepEqual(in, *detached) {
		t.Fatalf("detached batch mismatch\n want: %#v\n  got: %#v", in, *detached)
	}
}

// roundTrip encodes in via the heap-mode Marshal, decodes back into a
// fresh Batch, and returns the decoded value. Fatals on encode/decode
// errors. Used by every DeepEqual test in this file.
func roundTrip(t *testing.T, in *Batch) *Batch {
	t.Helper()
	w := gsbm.NewWriter(nil)
	if err := in.MarshalGSBM(w); err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got Batch
	if err := got.UnmarshalGSBM(gsbm.NewReader(w.Bytes())); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return &got
}

// emitEmptySlice mirrors the codegen's encode-of-nil-slice output: tag,
// outer length-delim, zero-count uvarint. Used to construct a full
// Batch wire image where only one tag carries data.
func emitEmptySlice(w *gsbm.Writer, tag uint32) {
	w.WriteTag(tag, gsbm.WireLengthDelim)
	m := w.BeginLengthDelim()
	w.WriteUvarint(0)
	w.EndLengthDelim(m)
}
