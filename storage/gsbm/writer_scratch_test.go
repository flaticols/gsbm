package gsbm

import (
	"bytes"
	"testing"
)

// The scratch cache is the foundation for materialize-once: a
// materializing codec (DecimalString, JSON, …) runs its gen function
// once per encode call, threaded across the size-mode → write-mode pass
// hand-off via adoptScratch. These tests pin three properties:
//
//   1. Same callsite → gen runs once, both call sites still emit the
//      same wire bytes.
//   2. Different callsites → gen runs per site (no cross-callsite
//      collapse).
//   3. Size-mode populate + adopt + write-mode emit produces identical
//      bytes to a single-pass write-mode emit. This is the property the
//      two-pass gsbm.Marshal flow relies on.

func TestWriteCachedStringSameCallsiteMaterializeOnce(t *testing.T) {
	const cs uintptr = 0x1
	const payload = "100.25"
	calls := 0
	gen := func() string {
		calls++
		return payload
	}

	w := NewWriter(nil)
	if err := w.WriteCachedString(cs, gen); err != nil {
		t.Fatalf("first call: %v", err)
	}
	first := append([]byte(nil), w.Bytes()...)

	if err := w.WriteCachedString(cs, gen); err != nil {
		t.Fatalf("second call: %v", err)
	}
	second := w.Bytes()[len(first):]

	if calls != 1 {
		t.Fatalf("gen invocations = %d, want 1 (materialize-once)", calls)
	}
	if !bytes.Equal(first, second) {
		t.Fatalf("cached call produced different bytes: first=%x second=%x", first, second)
	}

	// Sanity-check the wire shape against the canonical WriteString
	// path: cached output for the same string must match an uncached
	// WriteString.
	ref := NewWriter(nil)
	ref.WriteString(payload)
	if !bytes.Equal(first, ref.Bytes()) {
		t.Fatalf("cached wire bytes diverge from WriteString:\n cached=%x\n direct=%x", first, ref.Bytes())
	}
}

func TestWriteCachedStringDifferentCallsitesMaterializeIndependently(t *testing.T) {
	calls := map[uintptr]int{}
	mk := func(cs uintptr, s string) (uintptr, func() string) {
		return cs, func() string {
			calls[cs]++
			return s
		}
	}
	csA, genA := mk(0xa, "alpha")
	csB, genB := mk(0xb, "beta")

	w := NewWriter(nil)
	_ = w.WriteCachedString(csA, genA)
	_ = w.WriteCachedString(csB, genB)
	_ = w.WriteCachedString(csA, genA) // second hit on A → still 1 call

	if calls[csA] != 1 {
		t.Errorf("callsite A invocations = %d, want 1", calls[csA])
	}
	if calls[csB] != 1 {
		t.Errorf("callsite B invocations = %d, want 1", calls[csB])
	}

	ref := NewWriter(nil)
	ref.WriteString("alpha")
	ref.WriteString("beta")
	ref.WriteString("alpha")
	if !bytes.Equal(w.Bytes(), ref.Bytes()) {
		t.Fatalf("wire bytes mismatch:\n cached=%x\n direct=%x", w.Bytes(), ref.Bytes())
	}
}

func TestWriteCachedBytesMaterializeOnce(t *testing.T) {
	const cs uintptr = 0x5
	calls := 0
	gen := func() []byte {
		calls++
		return []byte{0xde, 0xad, 0xbe, 0xef}
	}

	w := NewWriter(nil)
	if err := w.WriteCachedBytes(cs, gen); err != nil {
		t.Fatalf("first call: %v", err)
	}
	if err := w.WriteCachedBytes(cs, gen); err != nil {
		t.Fatalf("second call: %v", err)
	}
	if calls != 1 {
		t.Fatalf("gen invocations = %d, want 1", calls)
	}

	ref := NewWriter(nil)
	ref.WriteBytes([]byte{0xde, 0xad, 0xbe, 0xef})
	ref.WriteBytes([]byte{0xde, 0xad, 0xbe, 0xef})
	if !bytes.Equal(w.Bytes(), ref.Bytes()) {
		t.Fatalf("wire bytes mismatch:\n cached=%x\n direct=%x", w.Bytes(), ref.Bytes())
	}
}

// TestWriteCachedSizeModeAccumulates pins that a size-mode Writer's
// cached-write accumulates the byte total exactly like the real-mode
// path. This is what lets gsbm.Marshal compute bodyLen from the
// size pass without re-materializing.
func TestWriteCachedSizeModeAccumulates(t *testing.T) {
	const cs uintptr = 0x42
	payload := "12345.6789"

	real := NewWriter(nil)
	_ = real.WriteCachedString(cs, func() string { return payload })

	size := NewCountingWriter()
	_ = size.WriteCachedString(cs, func() string { return payload })

	if got, want := size.Size(), len(real.Bytes()); got != want {
		t.Fatalf("size-mode Size=%d, real-mode len(Bytes)=%d", got, want)
	}
	if size.Bytes() != nil {
		t.Fatalf("size-mode Bytes() leaked: %d bytes", len(size.Bytes()))
	}
}

// TestAdoptScratchSizeToWriteHandoff is the critical mode-spanning
// property: size-mode populates the cache, real-mode adopts it, the
// second emit hits cache (gen invoked exactly once total), and the
// resulting wire bytes equal what a single-pass real-mode Writer would
// have produced.
func TestAdoptScratchSizeToWriteHandoff(t *testing.T) {
	const cs uintptr = 0x10a
	payload := "0.123456789"
	calls := 0
	gen := func() string {
		calls++
		return payload
	}

	// Pass A: size-mode populates cache, then real-mode adopts and writes.
	sizeW := NewCountingWriter()
	if err := sizeW.WriteCachedString(cs, gen); err != nil {
		t.Fatalf("size pass: %v", err)
	}
	bodyLen := sizeW.Size()

	bufW := NewWriter(make([]byte, 0, bodyLen))
	bufW.adoptScratch(sizeW)
	if err := bufW.WriteCachedString(cs, gen); err != nil {
		t.Fatalf("write pass: %v", err)
	}

	if calls != 1 {
		t.Fatalf("gen invocations across two-pass = %d, want 1 (cache must survive adopt)", calls)
	}
	if sizeW.scratch != nil {
		t.Errorf("donor scratch not cleared after adopt: %v", sizeW.scratch)
	}
	if bufW.scratch == nil {
		t.Error("receiver scratch nil after adopt")
	}

	// Pass B: single-pass real-mode reference.
	ref := NewWriter(nil)
	_ = ref.WriteCachedString(cs, func() string { return payload })

	if !bytes.Equal(bufW.Bytes(), ref.Bytes()) {
		t.Fatalf("adopt-scratch wire bytes diverge from single-pass:\n adopt=%x\n  ref=%x", bufW.Bytes(), ref.Bytes())
	}
	if got, want := len(bufW.Bytes()), bodyLen; got != want {
		t.Fatalf("write-mode body length=%d, size-mode predicted=%d", got, want)
	}
}

func TestAdoptScratchNilDonorIsNoop(t *testing.T) {
	w := NewWriter(nil)
	w.adoptScratch(nil) // must not panic; must not allocate scratch
	if w.scratch != nil {
		t.Fatalf("scratch initialized on nil adopt: %v", w.scratch)
	}
}

func TestWriteCachedRespectsStickyError(t *testing.T) {
	w := NewWriter(nil)
	w.WriteTag(0, WireVarint) // sets ErrZeroTag
	if w.Err() == nil {
		t.Fatal("expected sticky error after tag=0")
	}
	calls := 0
	gen := func() string {
		calls++
		return "x"
	}
	if err := w.WriteCachedString(0x1, gen); err == nil {
		t.Error("WriteCachedString returned nil despite sticky error")
	}
	if calls != 0 {
		t.Errorf("gen invoked despite sticky error: %d calls", calls)
	}
}

func TestResetClearsScratch(t *testing.T) {
	w := NewWriter(nil)
	calls := 0
	gen := func() string {
		calls++
		return "value"
	}
	_ = w.WriteCachedString(0x7, gen)
	if calls != 1 {
		t.Fatalf("pre-reset gen calls = %d, want 1", calls)
	}

	w.Reset(nil)
	_ = w.WriteCachedString(0x7, gen)
	if calls != 2 {
		t.Fatalf("post-reset gen calls = %d, want 2 (cache must clear on Reset)", calls)
	}
}
