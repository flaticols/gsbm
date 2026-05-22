package intwidth

import (
	"bytes"
	"errors"
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
// travels.
func TestLargeBoundaryRoundTrip(t *testing.T) {
	cases := []int{
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
		t.Run("", func(t *testing.T) {
			out := roundTrip(t, Record{Large: v})
			if out.Large != v {
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
		t.Run("", func(t *testing.T) {
			out := roundTrip(t, Record{Small: v})
			if out.Small != v {
				t.Fatalf("Small round-trip: got %d, want %d", out.Small, v)
			}
		})
	}
}

// TestSmallOverflow asserts that the explicit type=int32 marker still
// enforces the int32 bound at encode (and would at decode if such a blob
// arrived from elsewhere). Same error path as the un-annotated `int`
// field today.
func TestSmallOverflow(t *testing.T) {
	cases := []int{math.MaxInt32 + 1, math.MinInt32 - 1}
	for _, v := range cases {
		t.Run("", func(t *testing.T) {
			var w gsbm.Writer
			err := (&Record{Small: v}).MarshalGSBM(&w)
			if !errors.Is(err, gsbm.ErrIntegerOverflow) {
				t.Fatalf("Small=%d: got err=%v, want ErrIntegerOverflow", v, err)
			}
		})
	}
}

// TestBothFieldsRoundTrip exercises both fields together to confirm the
// per-field override is independent — Large taking a beyond-int32 value
// does not poison Small's bounds check, and Small at the int32 boundary
// does not affect Large's decoding.
func TestBothFieldsRoundTrip(t *testing.T) {
	in := Record{Small: math.MaxInt32, Large: math.MaxInt64}
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
	in := Record{Small: math.MaxInt32, Large: math.MaxInt64}
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
		small, large int
	}{
		{0, 0},
		{math.MaxInt32, math.MinInt32},
		{math.MinInt32, math.MaxInt32},
		{1, math.MaxInt64},
		{-1, math.MinInt64},
	}
	for _, c := range cases {
		t.Run("", func(t *testing.T) {
			got := encode(t, Record{Small: c.small, Large: c.large})

			var want gsbm.Writer
			want.WriteTag(1, gsbm.WireVarint)
			want.WriteVarint(int64(c.small))
			want.WriteTag(2, gsbm.WireVarint)
			want.WriteVarint(int64(c.large))
			if err := want.Err(); err != nil {
				t.Fatalf("expected-writer err: %v", err)
			}
			if !bytes.Equal(got, want.Bytes()) {
				t.Fatalf("wire shape drift for Small=%d Large=%d:\n got:  %x\n want: %x", c.small, c.large, got, want.Bytes())
			}
		})
	}
}
