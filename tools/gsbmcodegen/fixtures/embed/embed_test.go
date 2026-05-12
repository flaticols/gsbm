package embed

import (
	"bytes"
	"reflect"
	"testing"

	"go.flaticols.dev/gsbm/storage/gsbm"
)

// TestExtendedRoundTrip is the value-embed round-trip case: encoding an
// Extended whose Base.Total and Reason both carry data and decoding into a
// fresh Extended yields the original via DeepEqual.
func TestExtendedRoundTrip(t *testing.T) {
	in := Extended{
		Base:   Base{Total: 42},
		Reason: "answer",
	}
	w := gsbm.NewWriter(nil)
	if err := in.MarshalGSBM(w); err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got Extended
	if err := got.UnmarshalGSBM(gsbm.NewReader(w.Bytes())); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(in, got) {
		t.Fatalf("mismatch\n want: %#v\n  got: %#v", in, got)
	}
}

// TestExtendedWireMatchesFlat is the load-bearing wire-stability claim
// from issue #11: flattening must not change the on-wire bytes. Encoding
// Extended{Base: {Total: 42}, Reason: "x"} produces the same bytes as
// the hand-flattened Flat{Total: 42, Reason: "x"} carrying the same
// tags directly.
func TestExtendedWireMatchesFlat(t *testing.T) {
	ex := Extended{Base: Base{Total: 42}, Reason: "x"}
	flat := Flat{Total: 42, Reason: "x"}

	wEx := gsbm.NewWriter(nil)
	if err := ex.MarshalGSBM(wEx); err != nil {
		t.Fatalf("marshal extended: %v", err)
	}
	wFlat := gsbm.NewWriter(nil)
	if err := flat.MarshalGSBM(wFlat); err != nil {
		t.Fatalf("marshal flat: %v", err)
	}
	if !bytes.Equal(wEx.Bytes(), wFlat.Bytes()) {
		t.Fatalf("embed-flattened bytes differ from hand-flattened\n extended: %x\n     flat: %x", wEx.Bytes(), wFlat.Bytes())
	}
}

// TestFlatDecodesAsExtended pins the symmetric direction: a wire image
// produced by Flat decodes successfully into Extended (because the tag
// space is identical), and field values land correctly.
func TestFlatDecodesAsExtended(t *testing.T) {
	flat := Flat{Total: 7, Reason: "y"}
	w := gsbm.NewWriter(nil)
	if err := flat.MarshalGSBM(w); err != nil {
		t.Fatalf("marshal flat: %v", err)
	}
	var ex Extended
	if err := ex.UnmarshalGSBM(gsbm.NewReader(w.Bytes())); err != nil {
		t.Fatalf("unmarshal as extended: %v", err)
	}
	if ex.Base.Total != 7 || ex.Reason != "y" {
		t.Fatalf("decoded extended: %#v (want Total=7 Reason=y)", ex)
	}
}

// TestExtendedFieldPresent confirms the sidecar bitmap tracks flattened
// tags by tag number, not field path: tag 1 lives on Base.Total and
// FieldPresent(1) returns true after a successful decode.
func TestExtendedFieldPresent(t *testing.T) {
	in := Extended{Base: Base{Total: 1}, Reason: "z"}
	w := gsbm.NewWriter(nil)
	if err := in.MarshalGSBM(w); err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got Extended
	if err := got.UnmarshalGSBM(gsbm.NewReader(w.Bytes())); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !got.FieldPresent(1) {
		t.Errorf("FieldPresent(1) = false, want true (tag 1 = Total, flattened from Base)")
	}
	if !got.FieldPresent(2) {
		t.Errorf("FieldPresent(2) = false, want true (tag 2 = Reason, direct)")
	}
}

// TestExtendedReset confirms Reset zeros each promoted field through the
// access path (`v.Base.Total = 0`), since the embedded type itself has no
// generated Reset method.
func TestExtendedReset(t *testing.T) {
	v := Extended{Base: Base{Total: 99}, Reason: "stale"}
	v.Reset()
	if v.Base.Total != 0 || v.Reason != "" {
		t.Fatalf("after Reset: %#v (want zero)", v)
	}
}

// TestPtrExtendedNilRoundTrip pins the nil-pointer encoding contract:
// a nil *Base produces no flattened-tag bytes on the wire, and the
// decoded receiver leaves the pointer nil.
func TestPtrExtendedNilRoundTrip(t *testing.T) {
	in := PtrExtended{Reason: "alone"}
	w := gsbm.NewWriter(nil)
	if err := in.MarshalGSBM(w); err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got PtrExtended
	if err := got.UnmarshalGSBM(gsbm.NewReader(w.Bytes())); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Base != nil {
		t.Fatalf("Base: want nil, got %#v", got.Base)
	}
	if got.Reason != "alone" {
		t.Fatalf("Reason: want alone, got %q", got.Reason)
	}
}

// TestPtrExtendedNilWireOmitsFlattenedTags is the byte-level pin: the
// wire image for a nil-Base PtrExtended contains only the Reason tag —
// no orphan tag-1 key with empty body.
func TestPtrExtendedNilWireOmitsFlattenedTags(t *testing.T) {
	in := PtrExtended{Reason: "alone"}
	w := gsbm.NewWriter(nil)
	if err := in.MarshalGSBM(w); err != nil {
		t.Fatalf("marshal: %v", err)
	}

	want := gsbm.NewWriter(nil)
	want.WriteTag(2, gsbm.WireLengthDelim)
	want.WriteString("alone")
	if want.Err() != nil {
		t.Fatalf("want builder: %v", want.Err())
	}
	if !bytes.Equal(w.Bytes(), want.Bytes()) {
		t.Fatalf("nil-base wire bytes\n  got: %x\n want: %x", w.Bytes(), want.Bytes())
	}
}

// TestPtrExtendedZeroValueEmbedRoundTrip pins the allocated-but-all-zero
// case: when v.Base is non-nil but every flattened field carries the
// type's zero value, every flattened tag still appears on the wire (per
// the normal field rule — required primitives always emit). The decoded
// receiver sees v.Base allocated.
func TestPtrExtendedZeroValueEmbedRoundTrip(t *testing.T) {
	in := PtrExtended{Base: &Base{}, Reason: ""}
	w := gsbm.NewWriter(nil)
	if err := in.MarshalGSBM(w); err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got PtrExtended
	if err := got.UnmarshalGSBM(gsbm.NewReader(w.Bytes())); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Base == nil {
		t.Fatalf("expected Base allocated after decode, got nil")
	}
	if got.Base.Total != 0 || got.Reason != "" {
		t.Fatalf("decoded: %#v (want zero values)", got)
	}
}

// TestPtrExtendedPresentRoundTrip is the "happy path" for pointer embed:
// non-nil *Base with a non-zero Total decodes back equivalently.
func TestPtrExtendedPresentRoundTrip(t *testing.T) {
	in := PtrExtended{Base: &Base{Total: 100}, Reason: "set"}
	w := gsbm.NewWriter(nil)
	if err := in.MarshalGSBM(w); err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got PtrExtended
	if err := got.UnmarshalGSBM(gsbm.NewReader(w.Bytes())); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(in, got) {
		t.Fatalf("mismatch\n want: %#v\n  got: %#v", in, got)
	}
}

// TestPtrExtendedReset confirms Reset drops the embedded pointer to nil
// (rather than calling the non-existent Base.Reset).
func TestPtrExtendedReset(t *testing.T) {
	v := PtrExtended{Base: &Base{Total: 5}, Reason: "x"}
	v.Reset()
	if v.Base != nil {
		t.Fatalf("after Reset: Base = %#v, want nil", v.Base)
	}
	if v.Reason != "" {
		t.Fatalf("after Reset: Reason = %q, want empty", v.Reason)
	}
}

// TestDeepRoundTrip is the multi-level embed round-trip: Deep embeds Mid
// embeds Base; tags from all three levels round-trip via DeepEqual.
func TestDeepRoundTrip(t *testing.T) {
	in := Deep{
		Mid:    Mid{Base: Base{Total: 10}, Note: "mid-note"},
		Caller: "outer",
	}
	w := gsbm.NewWriter(nil)
	if err := in.MarshalGSBM(w); err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got Deep
	if err := got.UnmarshalGSBM(gsbm.NewReader(w.Bytes())); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(in, got) {
		t.Fatalf("mismatch\n want: %#v\n  got: %#v", in, got)
	}
}

// TestDeepFieldPresent — every flattened tag (1, 3, 4) marks presence at
// the outer level after decode. `got` is heap-allocated via newDeepHeap so
// its address is stable across Unmarshal and FieldPresent: the presence
// sidecar keys on the receiver's uintptr (intentionally, to avoid pinning
// via GC) and a stack-resident receiver can move under stack growth
// between the MarkPresent call inside Unmarshal and the later IsPresent
// lookup, producing a phantom missing-tag.
func TestDeepFieldPresent(t *testing.T) {
	in := Deep{Mid: Mid{Base: Base{Total: 1}, Note: "m"}, Caller: "c"}
	w := gsbm.NewWriter(nil)
	if err := in.MarshalGSBM(w); err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := newDeepHeap()
	if err := got.UnmarshalGSBM(gsbm.NewReader(w.Bytes())); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, tag := range []uint32{1, 3, 4} {
		if !got.FieldPresent(tag) {
			t.Errorf("FieldPresent(%d) = false, want true", tag)
		}
	}
}

// newDeepHeap allocates a fresh *Deep on the heap. The //go:noinline
// directive prevents the compiler from inlining the body and stack-
// allocating the result via escape analysis — see TestDeepFieldPresent
// for why a stable heap address matters.
//
//go:noinline
func newDeepHeap() *Deep { return &Deep{} }

// TestWrappedPtrResetZeroValue covers the value-embed-wrapping-pointer-embed
// case: a freshly-allocated WrappedPtr has v.PtrCarrier.Base == nil. A
// naive Reset implementation would emit `v.PtrCarrier.Base.Total = 0`
// because the outermost embed (PtrCarrier) is a value embed — that would
// panic. Reset must instead drop the inner pointer.
func TestWrappedPtrResetZeroValue(t *testing.T) {
	var v WrappedPtr
	v.Reset()
	if v.PtrCarrier.Base != nil {
		t.Fatalf("after Reset on zero value: Base = %#v, want nil", v.PtrCarrier.Base)
	}
}

// TestWrappedPtrResetPopulated mirrors the zero-value case for a populated
// receiver: Reset drops the *Base regardless of contents and clears the
// remaining flattened fields.
func TestWrappedPtrResetPopulated(t *testing.T) {
	v := WrappedPtr{
		PtrCarrier: PtrCarrier{Base: &Base{Total: 9}, Note: "n"},
		Caller:     "c",
	}
	v.Reset()
	if v.PtrCarrier.Base != nil {
		t.Fatalf("after Reset: Base = %#v, want nil", v.PtrCarrier.Base)
	}
	if v.PtrCarrier.Note != "" {
		t.Fatalf("after Reset: Note = %q, want empty", v.PtrCarrier.Note)
	}
	if v.Caller != "" {
		t.Fatalf("after Reset: Caller = %q, want empty", v.Caller)
	}
}

// TestWrappedPtrNilRoundTrip — encoding with a nil inner *Base elides
// every Base-flattened tag; decoding into a fresh receiver leaves Base nil.
func TestWrappedPtrNilRoundTrip(t *testing.T) {
	in := WrappedPtr{
		PtrCarrier: PtrCarrier{Base: nil, Note: "n"},
		Caller:     "c",
	}
	w := gsbm.NewWriter(nil)
	if err := in.MarshalGSBM(w); err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got WrappedPtr
	if err := got.UnmarshalGSBM(gsbm.NewReader(w.Bytes())); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.PtrCarrier.Base != nil {
		t.Fatalf("after decode: Base = %#v, want nil", got.PtrCarrier.Base)
	}
	if got.PtrCarrier.Note != "n" || got.Caller != "c" {
		t.Fatalf("flattened-from-value fields lost: %#v", got)
	}
}

// TestDoublePtrResetZeroValue is the regression pin for the two-consecutive-
// pointer-hop chain: Reset on a zero-value DoublePtr must not deref the
// outer nil *PtrMid in service of niling v.PtrMid.Base. Niling the
// outermost pointer alone is sufficient (every deeper hop is dropped with
// it). A naive Reset that emits both `v.PtrMid = nil` and
// `v.PtrMid.Base = nil` panics here.
func TestDoublePtrResetZeroValue(t *testing.T) {
	var v DoublePtr
	v.Reset()
	if v.PtrMid != nil {
		t.Fatalf("after Reset on zero value: PtrMid = %#v, want nil", v.PtrMid)
	}
}

// TestDoublePtrResetPopulated mirrors the zero-value case for a populated
// receiver: Reset drops the outermost *PtrMid (which takes *Base with it)
// without dereferencing any intermediate nil hop.
func TestDoublePtrResetPopulated(t *testing.T) {
	v := DoublePtr{
		PtrMid: &PtrMid{Base: &Base{Total: 7}},
		Caller: "c",
	}
	v.Reset()
	if v.PtrMid != nil {
		t.Fatalf("after Reset: PtrMid = %#v, want nil", v.PtrMid)
	}
	if v.Caller != "" {
		t.Fatalf("after Reset: Caller = %q, want empty", v.Caller)
	}
}

// TestDoublePtrNilRoundTrip — encoding with the outer *PtrMid nil elides
// every Base-flattened tag (tag 1); decoding into a fresh receiver leaves
// the entire pointer chain nil.
func TestDoublePtrNilRoundTrip(t *testing.T) {
	in := DoublePtr{Caller: "alone"}
	w := gsbm.NewWriter(nil)
	if err := in.MarshalGSBM(w); err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got DoublePtr
	if err := got.UnmarshalGSBM(gsbm.NewReader(w.Bytes())); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.PtrMid != nil {
		t.Fatalf("after decode: PtrMid = %#v, want nil", got.PtrMid)
	}
	if got.Caller != "alone" {
		t.Fatalf("Caller: want alone, got %q", got.Caller)
	}
}

// TestDoublePtrPresentRoundTrip — both hops populated round-trip
// correctly. The decoder must lazily allocate both *PtrMid and *Base on
// the incoming tag 1.
func TestDoublePtrPresentRoundTrip(t *testing.T) {
	in := DoublePtr{
		PtrMid: &PtrMid{Base: &Base{Total: 42}},
		Caller: "set",
	}
	w := gsbm.NewWriter(nil)
	if err := in.MarshalGSBM(w); err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got DoublePtr
	if err := got.UnmarshalGSBM(gsbm.NewReader(w.Bytes())); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(in, got) {
		t.Fatalf("mismatch\n want: %#v\n  got: %#v", in, got)
	}
}

// TestWrappedPtrPresentRoundTrip — encoding with a non-nil inner *Base
// emits Total; decoding lazily allocates v.PtrCarrier.Base on the incoming tag 1.
func TestWrappedPtrPresentRoundTrip(t *testing.T) {
	in := WrappedPtr{
		PtrCarrier: PtrCarrier{Base: &Base{Total: 100}, Note: "n"},
		Caller:     "c",
	}
	w := gsbm.NewWriter(nil)
	if err := in.MarshalGSBM(w); err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got WrappedPtr
	if err := got.UnmarshalGSBM(gsbm.NewReader(w.Bytes())); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(in, got) {
		t.Fatalf("mismatch\n want: %#v\n  got: %#v", in, got)
	}
}
