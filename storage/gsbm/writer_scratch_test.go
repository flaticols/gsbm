package gsbm

import (
	"bytes"
	"testing"
)

// The scratch cache is the foundation for materialize-once: a
// materializing codec (DecimalString, JSON, …) runs its gen function
// once per occurrence per gsbm.Marshal call, threaded across the
// size-mode → write-mode pass hand-off via adoptScratch. These tests
// pin three properties:
//
//   1. Within a single pass, distinct visits at the same callsite are
//      distinct occurrences — each materializes its own gen output.
//      This is what makes slice/map elements at the codegen-emitted
//      <struct,tag> constant safe.
//   2. Across the size→write pass hand-off, adoptScratch rewinds the
//      per-entry readIdx so the write pass replays the size pass's
//      occurrences in order without re-invoking gen.
//   3. Different callsites materialize independently (no cross-callsite
//      collapse).

func TestWriteCachedStringDistinctOccurrencesMaterializeIndependently(t *testing.T) {
	const cs uintptr = 0x1
	values := []string{"AAA", "BBB", "CCC"}
	calls := 0
	idx := 0
	gen := func() string {
		calls++
		s := values[idx]
		idx++
		return s
	}

	w := NewWriter(nil)
	for range values {
		if err := w.WriteCachedString(cs, gen); err != nil {
			t.Fatalf("WriteCachedString: %v", err)
		}
	}

	if calls != len(values) {
		t.Fatalf("gen invocations = %d, want %d (one per occurrence)", calls, len(values))
	}

	ref := NewWriter(nil)
	for _, v := range values {
		ref.WriteString(v)
	}
	if !bytes.Equal(w.Bytes(), ref.Bytes()) {
		t.Fatalf("wire bytes mismatch:\n cached=%x\n direct=%x", w.Bytes(), ref.Bytes())
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
	_ = w.WriteCachedString(csA, genA) // second occurrence at A → second materialization

	if calls[csA] != 2 {
		t.Errorf("callsite A invocations = %d, want 2 (one per occurrence)", calls[csA])
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

func TestWriteCachedBytesDistinctOccurrencesMaterializeIndependently(t *testing.T) {
	const cs uintptr = 0x5
	payloads := [][]byte{{0xde, 0xad}, {0xbe, 0xef}}
	calls := 0
	idx := 0
	gen := func() []byte {
		calls++
		p := payloads[idx]
		idx++
		return p
	}

	w := NewWriter(nil)
	for range payloads {
		if err := w.WriteCachedBytes(cs, gen); err != nil {
			t.Fatalf("WriteCachedBytes: %v", err)
		}
	}
	if calls != len(payloads) {
		t.Fatalf("gen invocations = %d, want %d", calls, len(payloads))
	}

	ref := NewWriter(nil)
	for _, p := range payloads {
		ref.WriteBytes(p)
	}
	if !bytes.Equal(w.Bytes(), ref.Bytes()) {
		t.Fatalf("wire bytes mismatch:\n cached=%x\n direct=%x", w.Bytes(), ref.Bytes())
	}
}

func TestWriteCachedBytesRespectsStickyError(t *testing.T) {
	w := NewWriter(nil)
	w.WriteTag(0, WireVarint) // sets ErrZeroTag
	if w.Err() == nil {
		t.Fatal("expected sticky error after tag=0")
	}
	calls := 0
	gen := func() []byte {
		calls++
		return []byte("x")
	}
	if err := w.WriteCachedBytes(0x1, gen); err == nil {
		t.Error("WriteCachedBytes returned nil despite sticky error")
	}
	if calls != 0 {
		t.Errorf("gen invoked despite sticky error: %d calls", calls)
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

// TestAdoptScratchPreservesOccurrenceOrder pins the slice/map-element
// case at the Writer level: the size pass records N distinct
// occurrences at one callsite, adoptScratch rewinds readIdx, and the
// write pass replays the same N occurrences in order — no gen
// re-invocation, no aliasing of later occurrences to the first one's
// bytes.
func TestAdoptScratchPreservesOccurrenceOrder(t *testing.T) {
	const cs uintptr = 0x2a
	payloads := []string{"AAA", "BBB", "CCC"}
	calls := 0
	idx := 0
	gen := func() string {
		calls++
		s := payloads[idx]
		idx++
		return s
	}

	sizeW := NewCountingWriter()
	for range payloads {
		if err := sizeW.WriteCachedString(cs, gen); err != nil {
			t.Fatalf("size pass: %v", err)
		}
	}
	bodyLen := sizeW.Size()
	if calls != len(payloads) {
		t.Fatalf("size-pass gen invocations = %d, want %d", calls, len(payloads))
	}

	bufW := NewWriter(make([]byte, 0, bodyLen))
	bufW.adoptScratch(sizeW)
	for range payloads {
		if err := bufW.WriteCachedString(cs, gen); err != nil {
			t.Fatalf("write pass: %v", err)
		}
	}
	if calls != len(payloads) {
		t.Fatalf("total gen invocations across two passes = %d, want %d (write pass must hit cache)", calls, len(payloads))
	}

	ref := NewWriter(nil)
	for _, p := range payloads {
		ref.WriteString(p)
	}
	if !bytes.Equal(bufW.Bytes(), ref.Bytes()) {
		t.Fatalf("two-pass bytes diverge from single-pass reference:\n two-pass=%x\n     ref=%x", bufW.Bytes(), ref.Bytes())
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
