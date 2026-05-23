package gsbm

import (
	"bytes"
	"math"
	"testing"
)

// TestWriteLengthByteWidths pins WriteLength's wire output across the
// varint width boundaries — a 1-byte length, the last 1-byte length, the
// first 2-byte length, and a 32-bit-max length that must occupy the full
// 5-byte slot. A regression here means SizeUvarint and the codegen-emitted
// SizeLengthDelim helpers no longer agree on prefix width.
func TestWriteLengthByteWidths(t *testing.T) {
	cases := []struct {
		name string
		n    int
		want []byte
	}{
		{"zero", 0, []byte{0x00}},
		{"one", 1, []byte{0x01}},
		{"max-1byte", 127, []byte{0x7f}},
		{"min-2byte", 128, []byte{0x80, 0x01}},
		{"max-2byte", 16383, []byte{0xff, 0x7f}},
		{"min-3byte", 16384, []byte{0x80, 0x80, 0x01}},
		{"maxInt32", math.MaxInt32, []byte{0xff, 0xff, 0xff, 0xff, 0x07}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := NewWriter(nil)
			w.WriteLength(tc.n)
			if err := w.Err(); err != nil {
				t.Fatalf("WriteLength(%d): %v", tc.n, err)
			}
			if !bytes.Equal(w.Bytes(), tc.want) {
				t.Fatalf("WriteLength(%d) = % x, want % x", tc.n, w.Bytes(), tc.want)
			}
		})
	}
}

// TestWriteLengthEquivalentToWriteUvarint locks in the contract from the
// doc-comment: WriteLength(n) emits the same bytes as
// WriteUvarint(uint64(n)) for every non-negative n. Generated code uses
// the two interchangeably (SizeUvarint counts what WriteLength writes),
// so any drift between them is a silent wire-format break.
func TestWriteLengthEquivalentToWriteUvarint(t *testing.T) {
	values := []int{0, 1, 2, 127, 128, 129, 16383, 16384, 1 << 20, math.MaxInt32}
	for _, n := range values {
		wl := NewWriter(nil)
		wl.WriteLength(n)
		wu := NewWriter(nil)
		wu.WriteUvarint(uint64(n))
		if !bytes.Equal(wl.Bytes(), wu.Bytes()) {
			t.Fatalf("WriteLength(%d) = % x, WriteUvarint = % x", n, wl.Bytes(), wu.Bytes())
		}
	}
}

// TestWriteLengthSizeMode pins the size-mode behavior: a counting
// writer's sizeAcc advances by SizeUvarint(uint64(n)), nothing more. The
// analytic SizeGSBM emit path relies on this exact accounting — if
// WriteLength bumped sizeAcc by a different amount than the body it
// prefixes, the size pass would disagree with the marshal pass.
func TestWriteLengthSizeMode(t *testing.T) {
	cases := []struct {
		n    int
		want int
	}{
		{0, 1}, {127, 1}, {128, 2}, {16383, 2}, {16384, 3}, {math.MaxInt32, 5},
	}
	for _, tc := range cases {
		w := NewCountingWriter()
		w.WriteLength(tc.n)
		if w.Size() != tc.want {
			t.Fatalf("size-mode WriteLength(%d): Size=%d, want %d", tc.n, w.Size(), tc.want)
		}
	}
}

// TestWriteLengthStreamingDoesNotConsumeStreamSizes pins the load-bearing
// distinction between WriteLength and BeginLengthDelim in streaming mode:
// BeginLengthDelim pops the next recorded body size from streamSizes,
// while WriteLength writes its caller-supplied length inline and leaves
// streamSizes untouched. Mixing the two — generated code uses
// WriteLength, handwritten remainders use BeginLengthDelim — depends on
// this invariant or the streaming pass will write the wrong length for
// the next BeginLengthDelim region it encounters.
func TestWriteLengthStreamingDoesNotConsumeStreamSizes(t *testing.T) {
	var out bytes.Buffer
	recorded := []int{42, 7} // reserved for two future BeginLengthDelim calls
	sw := newStreamingWriter(&out, recorded, 1<<10)

	sw.WriteLength(5)
	if err := sw.Err(); err != nil {
		t.Fatalf("WriteLength: %v", err)
	}
	if sw.streamSizeIdx != 0 {
		t.Fatalf("WriteLength consumed streamSizes: idx=%d, want 0", sw.streamSizeIdx)
	}

	// A subsequent BeginLengthDelim must still see the first recorded
	// size (42) — proving WriteLength left the queue intact.
	_ = sw.BeginLengthDelim()
	if sw.streamSizeIdx != 1 {
		t.Fatalf("BeginLengthDelim after WriteLength: idx=%d, want 1", sw.streamSizeIdx)
	}

	if err := sw.flushAll(); err != nil {
		t.Fatalf("flushAll: %v", err)
	}
	// Expected wire: WriteLength(5) → 0x05; BeginLengthDelim → 42 → 0x2a.
	if got, want := out.Bytes(), []byte{0x05, 0x2a}; !bytes.Equal(got, want) {
		t.Fatalf("streaming output = % x, want % x", got, want)
	}
}

// TestWriteLengthSizeModeNoRecordedRegions confirms WriteLength does NOT
// touch the recording-region stack. The whole point of the API is to let
// generated code emit a length prefix without growing recordedRegions on
// the streaming-compressed encode path. If WriteLength accidentally
// recorded a region, the win evaporates.
func TestWriteLengthSizeModeNoRecordedRegions(t *testing.T) {
	sw := newRecordingSizeWriter()
	sw.WriteLength(123)
	sw.WriteLength(0)
	sw.WriteLength(1 << 20)
	if got := sw.recordedRegionSizes(); len(got) != 0 {
		t.Fatalf("WriteLength recorded %d regions, want 0: %v", len(got), got)
	}
	if len(sw.regionStack) != 0 {
		t.Fatalf("WriteLength pushed %d region-stack frames, want 0", len(sw.regionStack))
	}
}

// TestWriteLengthNegativePanics asserts WriteLength panics on negative
// input. A negative length signals a bug in the caller's SizeGSBM (or
// arithmetic on its result) — silently encoding the two's-complement
// uint64 cast would corrupt every following field. Crash loud, crash
// early.
func TestWriteLengthNegativePanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("WriteLength(-1) did not panic")
		}
	}()
	w := NewWriter(nil)
	w.WriteLength(-1)
}

// TestWriteLengthShortVarintNotReserved confirms WriteLength writes the
// minimal varint width — not the 5-byte reserved slot that BeginLengthDelim
// uses. The codegen win comes from the analytic path emitting exactly the
// bytes that end up on the wire, with no shift-on-close.
func TestWriteLengthShortVarintNotReserved(t *testing.T) {
	w := NewWriter(nil)
	w.WriteLength(1)
	if got := len(w.Bytes()); got != 1 {
		t.Fatalf("WriteLength(1) wrote %d bytes, want 1", got)
	}
}
