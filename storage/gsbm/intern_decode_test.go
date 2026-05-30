package gsbm_test

import (
	"reflect"
	"testing"
	"unsafe"

	"go.flaticols.dev/gsbm/internal/bench"
	"go.flaticols.dev/gsbm/storage/gsbm"
	"go.flaticols.dev/gsbm/tools/gsbmcodegen/fixtures/sample"
)

// internedBlob marshals a rich sample.Order into a full blob (with header)
// for the interning decode tests. sample.Order exercises every composite
// shape the interner must compose with: plain/optional strings, slices of
// strings, string-keyed maps (map[string]int64, map[string]Label),
// named string-underlying types, and []byte.
func internedBlob(tb testing.TB) ([]byte, sample.Order) {
	tb.Helper()
	o := bench.MakeLargeOrder(0, 1<<20, 2<<20)
	blob, err := gsbm.Marshal(&o, 1)
	if err != nil {
		tb.Fatalf("Marshal: %v", err)
	}
	return blob, o
}

// TestInternedDecodeEqualsHeap is the correctness proof: a graph decoded
// with the interning allocator installed must be deep-equal to one decoded
// on the default heap path. Interning changes only string backing storage,
// never decoded values — and this asserts it across maps, slices, optional
// fields, named types, and []byte in one shot.
func TestInternedDecodeEqualsHeap(t *testing.T) {
	blob, _ := internedBlob(t)

	var heapDst sample.Order
	if err := gsbm.DecodeInto(blob, &heapDst); err != nil {
		t.Fatalf("heap DecodeInto: %v", err)
	}

	var internDst sample.Order
	if err := gsbm.DecodeInterned(blob, &internDst); err != nil {
		t.Fatalf("DecodeInterned: %v", err)
	}

	if !reflect.DeepEqual(heapDst, internDst) {
		t.Fatal("interned decode is not deep-equal to heap decode")
	}
}

// TestInternedDecodeManualFlowEqualsHeap pins the manual install path
// (NewReader + SetAllocator(interner) + ReadHeader + UnmarshalGSBM), the
// shape a caller uses for a shared cross-blob interner.
func TestInternedDecodeManualFlowEqualsHeap(t *testing.T) {
	blob, _ := internedBlob(t)

	var heapDst sample.Order
	if err := gsbm.DecodeInto(blob, &heapDst); err != nil {
		t.Fatalf("heap DecodeInto: %v", err)
	}

	in := gsbm.NewInterner()
	var dst sample.Order
	dst.Reset()
	r := gsbm.NewReader(blob)
	r.SetAllocator(in)
	if _, _, _, err := r.ReadHeader(); err != nil {
		t.Fatalf("ReadHeader: %v", err)
	}
	if err := dst.UnmarshalGSBM(r); err != nil {
		t.Fatalf("UnmarshalGSBM: %v", err)
	}
	if err := r.Err(); err != nil {
		t.Fatalf("reader err: %v", err)
	}
	if !reflect.DeepEqual(heapDst, dst) {
		t.Fatal("manual interned decode not deep-equal to heap decode")
	}
	if in.Len() == 0 {
		t.Fatal("interner retained no strings — decode did not route through AcquireString")
	}
}

// TestInternedMapKeysShareBacking proves the dedup actually happens and is
// safe for map keys: two map entries that decoded the same string value
// share one backing array, and the map still resolves both keys (interned
// strings are stable, unlike borrow-strings, so they are valid map keys).
func TestInternedMapKeysShareBacking(t *testing.T) {
	blob, _ := internedBlob(t)
	in := gsbm.NewInterner()
	var dst sample.Order
	r := gsbm.NewReader(blob)
	r.SetAllocator(in)
	if _, _, _, err := r.ReadHeader(); err != nil {
		t.Fatalf("ReadHeader: %v", err)
	}
	if err := dst.UnmarshalGSBM(r); err != nil {
		t.Fatalf("UnmarshalGSBM: %v", err)
	}

	// Collect every decoded string value and verify that equal values share
	// one backing pointer — the interning invariant on the live graph.
	backing := map[string]uintptr{}
	check := func(s string) {
		if len(s) == 0 {
			return
		}
		p := uintptr(unsafe.Pointer(unsafe.StringData(s)))
		if prev, ok := backing[s]; ok {
			if prev != p {
				t.Fatalf("equal strings %q have distinct backing — not interned", s)
			}
		} else {
			backing[s] = p
		}
	}
	for _, it := range dst.Items {
		check(it.SKU)
	}
	for k := range dst.Tags {
		check(k)
	}
	for k := range dst.Aliases {
		check(k)
	}
	// And the map must still resolve its keys correctly post-interning.
	for k, v := range dst.Tags {
		if got, ok := dst.Tags[k]; !ok || got != v {
			t.Fatalf("interned map key %q failed to resolve", k)
		}
	}
}

// TestInternedDecodeLowRepetitionNoCorruption documents that on a
// low-repetition payload (sample.Order draws mostly-distinct strings)
// interning is still correct but does not reduce allocations — the
// interner's table overhead is not recovered when there is little to fold.
// The decode-side WIN is measured on the repetition-heavy repeatednested
// fixture (see its BenchmarkDecodeRepeatedNested_Interned and
// TestInternedRetainedGraphShrinks); this test only guards that the
// no-win case stays correct rather than silently corrupting.
func TestInternedDecodeLowRepetitionNoCorruption(t *testing.T) {
	blob, _ := internedBlob(t)
	var heapDst, internDst sample.Order
	if err := gsbm.DecodeInto(blob, &heapDst); err != nil {
		t.Fatal(err)
	}
	if err := gsbm.DecodeInterned(blob, &internDst); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(heapDst, internDst) {
		t.Fatal("low-repetition interned decode diverged from heap decode")
	}
}
