package intwidth

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"testing"

	"go.flaticols.dev/gsbm/storage/gsbm"
)

// encode marshals r into a fresh Writer and returns a copy of the wire bytes.
func encode(t *testing.T, r Record) []byte {
	t.Helper()
	var w gsbm.Writer
	if err := r.MarshalGSBM(&w); err != nil {
		t.Fatalf("MarshalGSBM: %v", err)
	}
	return append([]byte(nil), w.Bytes()...)
}

func roundTrip(t *testing.T, in Record) Record {
	t.Helper()
	buf := encode(t, in)
	var out Record
	if err := out.UnmarshalGSBM(gsbm.NewReader(buf)); err != nil {
		t.Fatalf("UnmarshalGSBM: %v", err)
	}
	return out
}

// TestLargeBoundaryRoundTrip pins the type=int64 field across the int32
// boundary: every value in this table must encode without the int32 bounds
// check kicking in and decode back to itself bit-for-bit. The values that
// matter are MaxInt32+1 and MinInt32-1 — those would have failed before
// the override — and MaxInt64/MinInt64 to prove the full Go int64 range
// travels on a 64-bit host. Values are typed as int64 so the slice
// literal compiles on 32-bit; cases that overflow the platform `int`
// are skipped at runtime.
func TestLargeBoundaryRoundTrip(t *testing.T) {
	cases := []int64{
		0,
		1,
		-1,
		math.MaxInt32,
		math.MaxInt32 + 1,
		math.MinInt32,
		math.MinInt32 - 1,
		math.MaxInt64,
		math.MinInt64,
	}
	for _, v := range cases {
		t.Run(fmt.Sprintf("v=%d", v), func(t *testing.T) {
			if v > math.MaxInt || v < math.MinInt {
				t.Skipf("v=%d does not fit in platform int", v)
			}
			out := roundTrip(t, Record{Large: int(v)})
			if int64(out.Large) != v {
				t.Fatalf("Large round-trip: got %d, want %d", out.Large, v)
			}
		})
	}
}

// TestSmallBoundaryRoundTrip pins the type=int32 field: values inside the
// int32 range round-trip; values outside reject (see TestSmallOverflow).
func TestSmallBoundaryRoundTrip(t *testing.T) {
	cases := []int{0, 1, -1, math.MaxInt32, math.MinInt32}
	for _, v := range cases {
		t.Run(fmt.Sprintf("v=%d", v), func(t *testing.T) {
			out := roundTrip(t, Record{Small: v})
			if out.Small != v {
				t.Fatalf("Small round-trip: got %d, want %d", out.Small, v)
			}
		})
	}
}

// TestSmallOverflow asserts that the explicit type=int32 marker still
// enforces the int32 bound at encode and at decode. The encode side is
// covered by Marshal on a Record with an out-of-range Small. The decode
// side is covered by hand-rolling a wire blob carrying a varint above
// MaxInt32 at tag 1 (which the codec rejects via the int32 bounds check
// emitted in UnmarshalGSBM).
func TestSmallOverflow(t *testing.T) {
	if math.MaxInt < math.MaxInt64 {
		// Platform `int` is 32-bit; values outside the int32 range
		// can't be constructed in memory, so the encode-side check
		// is unreachable here. The decode-side blob test below still
		// runs.
		t.Log("platform int is 32-bit; skipping encode-side overflow cases")
	} else {
		cases := []int64{math.MaxInt32 + 1, math.MinInt32 - 1}
		for _, v := range cases {
			t.Run(fmt.Sprintf("encode/v=%d", v), func(t *testing.T) {
				var w gsbm.Writer
				err := (&Record{Small: int(v)}).MarshalGSBM(&w)
				if !errors.Is(err, gsbm.ErrIntegerOverflow) {
					t.Fatalf("Small=%d: got err=%v, want ErrIntegerOverflow", v, err)
				}
			})
		}
	}
	// Decode-side coverage: a blob crafted with a too-big varint at tag 1
	// must surface ErrIntegerOverflow. This protects against a future
	// codegen change that desyncs encode/decode bounds on the type=int32
	// path.
	t.Run("decode/MaxInt32+1", func(t *testing.T) {
		var w gsbm.Writer
		w.WriteTag(1, gsbm.WireVarint)
		w.WriteVarint(int64(math.MaxInt32) + 1)
		if err := w.Err(); err != nil {
			t.Fatalf("hand-rolled writer: %v", err)
		}
		blob := append([]byte(nil), w.Bytes()...)
		var out Record
		err := out.UnmarshalGSBM(gsbm.NewReader(blob))
		if !errors.Is(err, gsbm.ErrIntegerOverflow) {
			t.Fatalf("got err=%v, want ErrIntegerOverflow", err)
		}
	})
}

// TestBothFieldsRoundTrip exercises both fields together to confirm the
// per-field override is independent — Large taking a beyond-int32 value
// does not poison Small's bounds check, and Small at the int32 boundary
// does not affect Large's decoding. The Large=MaxInt64 leg only runs on
// 64-bit hosts where the platform `int` can hold it.
func TestBothFieldsRoundTrip(t *testing.T) {
	if math.MaxInt < math.MaxInt64 {
		t.Skip("platform int is 32-bit; MaxInt64 does not fit in Large (int)")
	}
	// Go through int64 vars so the int conversions are not evaluated
	// at compile time on 32-bit hosts (where MaxInt64 overflows int).
	var maxI32, maxI64 int64 = math.MaxInt32, math.MaxInt64
	in := Record{Small: int(maxI32), Large: int(maxI64)}
	out := roundTrip(t, in)
	if out != in {
		t.Fatalf("round-trip: got %+v, want %+v", out, in)
	}
}

// TestMarshalUnmarshalZeroAlloc pins the hot path: with a pre-sized
// Writer buffer reused via Reset, MarshalGSBM and UnmarshalGSBM on the
// override-bearing Record allocate zero per op. The override is wire-level
// only (a missing bounds-check branch on the int64 path; nothing else),
// so it must not introduce any allocation that the un-annotated `int`
// path doesn't already incur — and the un-annotated path is 0.
func TestMarshalUnmarshalZeroAlloc(t *testing.T) {
	if math.MaxInt < math.MaxInt64 {
		t.Skip("platform int is 32-bit; MaxInt64 does not fit in Large (int)")
	}
	var maxI32, maxI64 int64 = math.MaxInt32, math.MaxInt64
	in := Record{Small: int(maxI32), Large: int(maxI64)}
	buf := make([]byte, 0, 64)
	w := gsbm.NewWriter(buf)

	encAllocs := testing.AllocsPerRun(100, func() {
		w.Reset(buf)
		if err := in.MarshalGSBM(w); err != nil {
			t.Fatalf("MarshalGSBM: %v", err)
		}
	})
	if encAllocs != 0 {
		t.Errorf("MarshalGSBM: %.1f allocs/op, want 0", encAllocs)
	}

	w.Reset(buf)
	if err := in.MarshalGSBM(w); err != nil {
		t.Fatalf("seed MarshalGSBM: %v", err)
	}
	blob := append([]byte(nil), w.Bytes()...)

	var out Record
	decAllocs := testing.AllocsPerRun(100, func() {
		out = Record{}
		if err := out.UnmarshalGSBM(gsbm.NewReader(blob)); err != nil {
			t.Fatalf("UnmarshalGSBM: %v", err)
		}
	})
	if decAllocs != 0 {
		t.Errorf("UnmarshalGSBM: %.1f allocs/op, want 0", decAllocs)
	}
	t.Logf("encode=%.1f allocs/op, decode=%.1f allocs/op", encAllocs, decAllocs)
}

// TestWireShapeMatchesIntrinsicVarint pins the no-op-ness of type=int32
// and the int64-shape of type=int64: a Record{Small: v, Large: w} encodes
// exactly as `tag1varint(v) ++ tag2varint(w)`, using the same WriteVarint
// the codec uses. If either path drifted (e.g. an extra header or a zigzag
// switch), this test would notice.
//
// Cross-version contract: blobs from this codec must be readable by any
// reader that speaks varint at tags 1 and 2 — which is exactly the shape
// a plain Go `int64` field at those tags would produce today.
func TestWireShapeMatchesIntrinsicVarint(t *testing.T) {
	cases := []struct {
		small, large int64
	}{
		{0, 0},
		{math.MaxInt32, math.MinInt32},
		{math.MinInt32, math.MaxInt32},
		{1, math.MaxInt64},
		{-1, math.MinInt64},
	}
	for _, c := range cases {
		t.Run(fmt.Sprintf("small=%d,large=%d", c.small, c.large), func(t *testing.T) {
			if c.large > math.MaxInt || c.large < math.MinInt {
				t.Skipf("large=%d does not fit in platform int", c.large)
			}
			got := encode(t, Record{Small: int(c.small), Large: int(c.large)})

			var want gsbm.Writer
			want.WriteTag(1, gsbm.WireVarint)
			want.WriteVarint(c.small)
			want.WriteTag(2, gsbm.WireVarint)
			want.WriteVarint(c.large)
			if err := want.Err(); err != nil {
				t.Fatalf("expected-writer err: %v", err)
			}
			if !bytes.Equal(got, want.Bytes()) {
				t.Fatalf("wire shape drift for Small=%d Large=%d:\n got:  %x\n want: %x", c.small, c.large, got, want.Bytes())
			}
		})
	}
}
