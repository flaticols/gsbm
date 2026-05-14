package gsbm

import (
	"bytes"
	"errors"
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
//      per-lane cursors so the write pass replays the size pass's
//      occurrences in order without re-invoking gen.
//   3. Different callsites materialize independently (no cross-callsite
//      collapse).

func TestWriteCachedStringDistinctOccurrencesMaterializeIndependently(t *testing.T) {
	const cs uint64 = 0x1
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
	calls := map[uint64]int{}
	mk := func(cs uint64, s string) (uint64, func() string) {
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
	const cs uint64 = 0x5
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
	const cs uint64 = 0x42
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
	const cs uint64 = 0x10a
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
	const cs uint64 = 0x2a
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

// TestWriteCachedStringNoConversionAlloc pins the issue #29 fix:
// caching the gen-returned string directly (no []byte conversion) means
// WriteCachedString's first-occurrence cost equals WriteCachedBytes's
// when both are handed a value the cache can retain unchanged. Before
// the fix WriteCachedString did `[]byte(gen())`, adding one heap alloc
// per first occurrence relative to the bytes path.
func TestWriteCachedStringNoConversionAlloc(t *testing.T) {
	const cs uint64 = 0x1
	stringGen := func() string { return "constant-string-value" }
	cached := []byte("constant-string-value")
	bytesGen := func() []byte { return cached }

	strAllocs := testing.AllocsPerRun(100, func() {
		w := NewCountingWriter()
		_ = w.WriteCachedString(cs, stringGen)
	})
	bytesAllocs := testing.AllocsPerRun(100, func() {
		w := NewCountingWriter()
		_ = w.WriteCachedBytes(cs, bytesGen)
	})
	if strAllocs > bytesAllocs {
		t.Fatalf("WriteCachedString allocs/op = %.1f, WriteCachedBytes = %.1f — string path is allocating extra (likely []byte conversion regression)", strAllocs, bytesAllocs)
	}
}

// TestWriteCachedStringSharesStringStorage pins that the cached string
// is the gen-returned string identity, not a copy. Two writes at the
// same callsite (across pass hand-off) see the same string header.
func TestWriteCachedStringSharesStringStorage(t *testing.T) {
	const cs uint64 = 0xabc
	produced := "shared-string-value"
	calls := 0
	gen := func() string {
		calls++
		return produced
	}

	sizeW := NewCountingWriter()
	_ = sizeW.WriteCachedString(cs, gen)

	bufW := NewWriter(nil)
	bufW.adoptScratch(sizeW)
	_ = bufW.WriteCachedString(cs, gen)

	if calls != 1 {
		t.Fatalf("gen invocations across two-pass = %d, want 1", calls)
	}
	e := bufW.scratch[cs]
	if len(e.strings) != 1 {
		t.Fatalf("strings slot len = %d, want 1", len(e.strings))
	}
	if e.strings[0] != produced {
		t.Fatalf("cached string = %q, want %q", e.strings[0], produced)
	}
	// values slot should be untouched by the string path.
	if len(e.values) != 0 {
		t.Fatalf("values slot polluted by string path: len=%d", len(e.values))
	}
}

// TestWriteCachedAppendBytesDistinctOccurrencesMaterializeIndependently
// pins the per-occurrence semantics for the append-style cached writer:
// repeated visits at the same callsite each invoke appendFn and each
// occurrence gets its own materialization slot.
func TestWriteCachedAppendBytesDistinctOccurrencesMaterializeIndependently(t *testing.T) {
	const cs uint64 = 0x11
	payloads := []string{"alpha", "beta", "gamma"}
	calls := 0
	idx := 0
	appendFn := func(dst []byte) ([]byte, error) {
		calls++
		s := payloads[idx]
		idx++
		return append(dst, s...), nil
	}

	w := NewWriter(nil)
	for range payloads {
		if err := w.WriteCachedAppendBytes(cs, appendFn); err != nil {
			t.Fatalf("WriteCachedAppendBytes: %v", err)
		}
	}
	if calls != len(payloads) {
		t.Fatalf("appendFn invocations = %d, want %d (one per occurrence)", calls, len(payloads))
	}

	ref := NewWriter(nil)
	for _, p := range payloads {
		ref.WriteBytes([]byte(p))
	}
	if !bytes.Equal(w.Bytes(), ref.Bytes()) {
		t.Fatalf("wire bytes mismatch:\n cached=%x\n direct=%x", w.Bytes(), ref.Bytes())
	}
}

// TestWriteCachedAppendBytesDifferentCallsitesIndependent pins that
// distinct callsites maintain independent caches.
func TestWriteCachedAppendBytesDifferentCallsitesIndependent(t *testing.T) {
	calls := map[uint64]int{}
	mk := func(cs uint64, s string) (uint64, func(dst []byte) ([]byte, error)) {
		return cs, func(dst []byte) ([]byte, error) {
			calls[cs]++
			return append(dst, s...), nil
		}
	}
	csA, fnA := mk(0xa, "alpha")
	csB, fnB := mk(0xb, "beta")

	w := NewWriter(nil)
	_ = w.WriteCachedAppendBytes(csA, fnA)
	_ = w.WriteCachedAppendBytes(csB, fnB)
	_ = w.WriteCachedAppendBytes(csA, fnA)

	if calls[csA] != 2 {
		t.Errorf("callsite A appendFn invocations = %d, want 2", calls[csA])
	}
	if calls[csB] != 1 {
		t.Errorf("callsite B appendFn invocations = %d, want 1", calls[csB])
	}

	ref := NewWriter(nil)
	ref.WriteBytes([]byte("alpha"))
	ref.WriteBytes([]byte("beta"))
	ref.WriteBytes([]byte("alpha"))
	if !bytes.Equal(w.Bytes(), ref.Bytes()) {
		t.Fatalf("wire bytes mismatch:\n cached=%x\n direct=%x", w.Bytes(), ref.Bytes())
	}
}

// TestWriteCachedAppendBytesAdoptScratchHandoff pins the materialize-once
// invariant for the append path across the size→write pass hand-off.
func TestWriteCachedAppendBytesAdoptScratchHandoff(t *testing.T) {
	const cs uint64 = 0x20a
	payload := "12345.6789"
	calls := 0
	appendFn := func(dst []byte) ([]byte, error) {
		calls++
		return append(dst, payload...), nil
	}

	sizeW := NewCountingWriter()
	if err := sizeW.WriteCachedAppendBytes(cs, appendFn); err != nil {
		t.Fatalf("size pass: %v", err)
	}
	bodyLen := sizeW.Size()

	bufW := NewWriter(make([]byte, 0, bodyLen))
	bufW.adoptScratch(sizeW)
	if err := bufW.WriteCachedAppendBytes(cs, appendFn); err != nil {
		t.Fatalf("write pass: %v", err)
	}

	if calls != 1 {
		t.Fatalf("appendFn invocations across two passes = %d, want 1", calls)
	}

	ref := NewWriter(nil)
	ref.WriteBytes([]byte(payload))
	if !bytes.Equal(bufW.Bytes(), ref.Bytes()) {
		t.Fatalf("adopt-scratch wire bytes diverge from single-pass:\n adopt=%x\n  ref=%x", bufW.Bytes(), ref.Bytes())
	}
	if got, want := len(bufW.Bytes()), bodyLen; got != want {
		t.Fatalf("write-mode body length=%d, size-mode predicted=%d", got, want)
	}
}

// TestWriteCachedAppendBytesAdoptScratchPreservesOrder is the
// slice/map-element case at the Writer level for the append path: N
// distinct occurrences at one callsite replay in order across the
// hand-off, with appendFn called exactly N times in total.
func TestWriteCachedAppendBytesAdoptScratchPreservesOrder(t *testing.T) {
	const cs uint64 = 0x2b
	payloads := []string{"AAA", "BBB", "CCC"}
	calls := 0
	idx := 0
	appendFn := func(dst []byte) ([]byte, error) {
		calls++
		s := payloads[idx]
		idx++
		return append(dst, s...), nil
	}

	sizeW := NewCountingWriter()
	for range payloads {
		if err := sizeW.WriteCachedAppendBytes(cs, appendFn); err != nil {
			t.Fatalf("size pass: %v", err)
		}
	}
	if calls != len(payloads) {
		t.Fatalf("size-pass appendFn invocations = %d, want %d", calls, len(payloads))
	}

	bufW := NewWriter(nil)
	bufW.adoptScratch(sizeW)
	for range payloads {
		if err := bufW.WriteCachedAppendBytes(cs, appendFn); err != nil {
			t.Fatalf("write pass: %v", err)
		}
	}
	if calls != len(payloads) {
		t.Fatalf("total appendFn invocations across two passes = %d, want %d (write pass must hit cache)", calls, len(payloads))
	}

	ref := NewWriter(nil)
	for _, p := range payloads {
		ref.WriteBytes([]byte(p))
	}
	if !bytes.Equal(bufW.Bytes(), ref.Bytes()) {
		t.Fatalf("two-pass bytes diverge from single-pass reference:\n two-pass=%x\n     ref=%x", bufW.Bytes(), ref.Bytes())
	}
}

func TestWriteCachedAppendBytesRespectsStickyError(t *testing.T) {
	w := NewWriter(nil)
	w.WriteTag(0, WireVarint) // sets ErrZeroTag
	if w.Err() == nil {
		t.Fatal("expected sticky error after tag=0")
	}
	calls := 0
	appendFn := func(dst []byte) ([]byte, error) {
		calls++
		return append(dst, 'x'), nil
	}
	if err := w.WriteCachedAppendBytes(0x1, appendFn); err == nil {
		t.Error("WriteCachedAppendBytes returned nil despite sticky error")
	}
	if calls != 0 {
		t.Errorf("appendFn invoked despite sticky error: %d calls", calls)
	}
}

// TestWriteCachedAppendBytesPropagatesAppendError pins that an error
// returned by appendFn is surfaced via the Writer's sticky error state
// and short-circuits subsequent writes.
func TestWriteCachedAppendBytesPropagatesAppendError(t *testing.T) {
	w := NewWriter(nil)
	sentinel := errors.New("append failed")
	calls := 0
	appendFn := func(dst []byte) ([]byte, error) {
		calls++
		return nil, sentinel
	}
	err := w.WriteCachedAppendBytes(0x33, appendFn)
	if !errors.Is(err, sentinel) {
		t.Fatalf("WriteCachedAppendBytes err = %v, want %v", err, sentinel)
	}
	if !errors.Is(w.Err(), sentinel) {
		t.Fatalf("Writer sticky err = %v, want %v", w.Err(), sentinel)
	}
	if calls != 1 {
		t.Fatalf("appendFn calls = %d, want 1", calls)
	}
	// subsequent writes short-circuit: appendFn must not be invoked again.
	if err := w.WriteCachedAppendBytes(0x44, appendFn); !errors.Is(err, sentinel) {
		t.Fatalf("subsequent WriteCachedAppendBytes did not short-circuit: %v", err)
	}
	if calls != 1 {
		t.Fatalf("appendFn re-invoked after sticky error: calls = %d", calls)
	}
	// cache must not retain the failed entry.
	if e := w.scratch[0x33]; e != nil && len(e.values) != 0 {
		t.Fatalf("cache retained failed entry: %v", e.values)
	}
}

// TestWriteCachedAppendBytesSizeModeAccumulates pins that a size-mode
// Writer's append-cached write accumulates the byte total exactly like
// the real-mode path.
func TestWriteCachedAppendBytesSizeModeAccumulates(t *testing.T) {
	const cs uint64 = 0x42
	payload := "12345.6789"
	appendFn := func(dst []byte) ([]byte, error) {
		return append(dst, payload...), nil
	}

	real := NewWriter(nil)
	_ = real.WriteCachedAppendBytes(cs, appendFn)

	size := NewCountingWriter()
	_ = size.WriteCachedAppendBytes(cs, appendFn)

	if got, want := size.Size(), len(real.Bytes()); got != want {
		t.Fatalf("size-mode Size=%d, real-mode len(Bytes)=%d", got, want)
	}
}

// TestWriteCachedAppendBytesAllocBudget pins the allocation profile of
// the append path: the first-occurrence cost is at most one alloc above
// WriteCachedBytes (which receives a pre-formed slice) — i.e. one alloc
// for the appended backing array, not the extra string materialization
// that WriteCachedString would pay. Cached occurrences must be
// zero-alloc.
func TestWriteCachedAppendBytesAllocBudget(t *testing.T) {
	const cs uint64 = 0x55
	payload := "constant-append-value"
	appendFn := func(dst []byte) ([]byte, error) {
		return append(dst, payload...), nil
	}
	cached := []byte(payload)
	bytesGen := func() []byte { return cached }

	appendAllocs := testing.AllocsPerRun(100, func() {
		w := NewCountingWriter()
		_ = w.WriteCachedAppendBytes(cs, appendFn)
	})
	bytesAllocs := testing.AllocsPerRun(100, func() {
		w := NewCountingWriter()
		_ = w.WriteCachedBytes(cs, bytesGen)
	})
	// Append path materializes the slice itself; bytes path doesn't.
	// Budget: at most one extra alloc relative to the bytes baseline.
	if appendAllocs > bytesAllocs+1 {
		t.Fatalf("WriteCachedAppendBytes allocs/op = %.1f, WriteCachedBytes = %.1f — append path budget exceeded", appendAllocs, bytesAllocs)
	}

	// Cached-occurrence cost: zero allocs on the rewind/replay path.
	sizeW := NewCountingWriter()
	_ = sizeW.WriteCachedAppendBytes(cs, appendFn)
	bufW := NewWriter(make([]byte, 0, sizeW.Size()))
	bufW.adoptScratch(sizeW)

	cachedAllocs := testing.AllocsPerRun(100, func() {
		e := bufW.scratch[cs]
		e.valuesIdx = 0
		_ = bufW.WriteCachedAppendBytes(cs, appendFn)
		bufW.buf = bufW.buf[:0]
	})
	if cachedAllocs != 0 {
		t.Fatalf("WriteCachedAppendBytes cached-occurrence allocs/op = %.1f, want 0", cachedAllocs)
	}
}

// TestMixedHelpersAtSameCallsite covers a handwritten codec that uses
// both WriteCachedString and the byte-flavored cached helpers at the
// same callsite. The string and bytes lanes carry independent cursors,
// so the write pass replays each lane in order without re-materializing
// the second lane.
func TestMixedHelpersAtSameCallsite(t *testing.T) {
	const cs uint64 = 0x9
	stringCalls := 0
	bytesCalls := 0
	appendCalls := 0
	stringGen := func() string {
		stringCalls++
		return "string-payload"
	}
	bytesGen := func() []byte {
		bytesCalls++
		return []byte("bytes-payload")
	}
	appendFn := func(dst []byte) ([]byte, error) {
		appendCalls++
		return append(dst, "append-payload"...), nil
	}

	body := func(w *Writer) {
		_ = w.WriteCachedString(cs, stringGen)
		_ = w.WriteCachedBytes(cs, bytesGen)
		_ = w.WriteCachedAppendBytes(cs, appendFn)
		_ = w.WriteCachedString(cs, stringGen)
		_ = w.WriteCachedBytes(cs, bytesGen)
	}

	sizeW := NewCountingWriter()
	body(sizeW)
	if stringCalls != 2 || bytesCalls != 2 || appendCalls != 1 {
		t.Fatalf("size pass materializations = (string=%d, bytes=%d, append=%d), want (2, 2, 1)", stringCalls, bytesCalls, appendCalls)
	}

	bufW := NewWriter(make([]byte, 0, sizeW.Size()))
	bufW.adoptScratch(sizeW)
	body(bufW)
	if stringCalls != 2 || bytesCalls != 2 || appendCalls != 1 {
		t.Fatalf("write pass re-materialized despite cache: (string=%d, bytes=%d, append=%d), want (2, 2, 1)", stringCalls, bytesCalls, appendCalls)
	}

	ref := NewWriter(nil)
	ref.WriteString("string-payload")
	ref.WriteBytes([]byte("bytes-payload"))
	ref.WriteBytes([]byte("append-payload"))
	ref.WriteString("string-payload")
	ref.WriteBytes([]byte("bytes-payload"))
	if !bytes.Equal(bufW.Bytes(), ref.Bytes()) {
		t.Fatalf("mixed-helper wire bytes diverge:\n got=%x\n want=%x", bufW.Bytes(), ref.Bytes())
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
