package gsbm_test

import (
	"bytes"
	"sync"
	"testing"

	"go.flaticols.dev/gsbm/internal/bench"
	"go.flaticols.dev/gsbm/internal/bench/largepayload"
	"go.flaticols.dev/gsbm/internal/bench/money"
	"go.flaticols.dev/gsbm/storage/gsbm"
)

// largeOrderTargetMin / largeOrderTargetMax bracket the body byte size of
// the benchmark payload. The plan targets the 1-2 MiB Spanner offer-batch
// hot path; deviating from this range invalidates the locked alloc
// budgets recorded below.
const (
	largeOrderTargetMin = 1 << 20
	largeOrderTargetMax = 2 << 20
)

// pooledEncodeBudget is the encode-warm allocation ceiling under a
// caller-supplied buffer pool. The "1 alloc/op encode" target is the
// *gsbm.Writer struct itself; the codegen emits
// one extra `make([]string, 0, len(m))` per map field for the sorted-
// key buffer (spec §5.3 deterministic-key wire). sample.Order has two
// map fields (Tags, Aliases), so the steady-state floor is 3 allocs/op.
// 4 leaves one slot of slack for any one-off pool-interface boxing the
// runtime introduces under -race.
const pooledEncodeBudget = 4.0

// freshEncodeBudget is the encode-warm allocation ceiling when each
// iteration starts from a nil buffer. It bounds the *Writer alloc, the
// two map-key slices (see pooledEncodeBudget), plus the O(log n)
// buffer-growth allocations Go's append performs to reach ~1.5 MiB.
// Empirically locked at 37 allocs/op on the 1-2 MiB MakeLargeOrder
// payload (Go 1.26 append-growth strategy); 48 leaves ~25% headroom
// for harmless append-growth jitter while still catching a regression
// that doubles the count.
const freshEncodeBudget = 48.0

// newEncodeFixture builds the 1-2 MiB Order, marshals it once to learn
// the encoded length, and returns the value, a slice ready to be reset
// with [:0] inside the bench loop, and the body size in bytes (for
// passing to WriteHeader). seed=0 keeps fixtures reproducible across
// runs.
func newEncodeFixture(tb testing.TB) (m bench.Marshaler, sized []byte, bodyLen uint32) {
	tb.Helper()
	o := bench.MakeLargeOrder(0, largeOrderTargetMin, largeOrderTargetMax)
	m = &o
	size, err := bench.EncodedSize(m)
	if err != nil {
		tb.Fatalf("EncodedSize: %v", err)
	}
	if size < largeOrderTargetMin || size > largeOrderTargetMax {
		tb.Fatalf("MakeLargeOrder size %d out of range [%d, %d]", size, largeOrderTargetMin, largeOrderTargetMax)
	}
	// HeaderSize bytes for the header plus body; round up modestly so a
	// few extra bytes don't force a single grow on the first warm
	// iteration.
	sized = make([]byte, 0, size+64)
	return m, sized, uint32(size)
}

func BenchmarkLargeOrderEncodeHeapPooled(b *testing.B) {
	m, _, bodyLen := newEncodeFixture(b)
	pool := sync.Pool{New: func() any {
		buf := make([]byte, 0, largeOrderTargetMax+64)
		return &buf
	}}
	// Pre-warm the pool so the first iteration sees a properly sized
	// buffer. Without the warm-up the first op in a long run would
	// dominate the AllocsPerRun average.
	{
		bp := pool.Get().(*[]byte)
		w := gsbm.NewWriter((*bp)[:0])
		w.WriteHeader(0, 1, bodyLen)
		if err := m.MarshalGSBM(w); err != nil {
			b.Fatal(err)
		}
		*bp = w.Bytes()
		pool.Put(bp)
	}
	b.ReportAllocs()
	for b.Loop() {
		bp := pool.Get().(*[]byte)
		w := gsbm.NewWriter((*bp)[:0])
		w.WriteHeader(0, 1, bodyLen)
		if err := m.MarshalGSBM(w); err != nil {
			b.Fatal(err)
		}
		*bp = w.Bytes()
		pool.Put(bp)
	}
}

func BenchmarkLargeOrderEncodeHeapFresh(b *testing.B) {
	m, _, bodyLen := newEncodeFixture(b)
	b.ReportAllocs()
	for b.Loop() {
		w := gsbm.NewWriter(nil)
		w.WriteHeader(0, 1, bodyLen)
		if err := m.MarshalGSBM(w); err != nil {
			b.Fatal(err)
		}
		_ = w.Bytes()
	}
}

// BenchmarkMarshalLargeOrder measures the gsbm.Marshal exact-allocation
// encode path on the same 1-2 MiB Order payload used by the pooled and
// fresh encode benchmarks. Marshal uses v.SizeGSBM() to size the buffer
// up front, so a single make is the only buffer alloc; the *Writer
// struct and the codegen's map-key scratch slices contribute the rest.
func BenchmarkMarshalLargeOrder(b *testing.B) {
	m, _, _ := newEncodeFixture(b)
	// gsbm.Marshal takes the gsbm.Marshaler interface — bench.Marshaler
	// only declares MarshalGSBM. The fixture root (sample.Order) has
	// the full SizeGSBM+MarshalGSBM contract on its pointer; cast.
	gm, ok := m.(gsbm.Marshaler)
	if !ok {
		b.Fatalf("fixture does not satisfy gsbm.Marshaler: %T", m)
	}
	// Warm-up so per-Marshal one-shot initializations aren't charged.
	if _, err := gsbm.Marshal(gm, 1); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := gsbm.Marshal(gm, 1); err != nil {
			b.Fatal(err)
		}
	}
}

// TestBenchmarkLargeOrderEncodeHeapPooledBudget asserts the warm-pool
// encode path stays within the documented 1-alloc/op target. The
// pooled buffer absorbs the byte-slice growth; the only allocation
// per iteration should be the *gsbm.Writer struct itself.
func TestBenchmarkLargeOrderEncodeHeapPooledBudget(t *testing.T) {
	if testing.Short() {
		t.Skip("alloc budget runs full")
	}
	m, _, bodyLen := newEncodeFixture(t)
	pool := sync.Pool{New: func() any {
		buf := make([]byte, 0, largeOrderTargetMax+64)
		return &buf
	}}
	{
		bp := pool.Get().(*[]byte)
		w := gsbm.NewWriter((*bp)[:0])
		w.WriteHeader(0, 1, bodyLen)
		if err := m.MarshalGSBM(w); err != nil {
			t.Fatal(err)
		}
		*bp = w.Bytes()
		pool.Put(bp)
	}
	avg := testing.AllocsPerRun(20, func() {
		bp := pool.Get().(*[]byte)
		w := gsbm.NewWriter((*bp)[:0])
		w.WriteHeader(0, 1, bodyLen)
		if err := m.MarshalGSBM(w); err != nil {
			t.Fatal(err)
		}
		*bp = w.Bytes()
		pool.Put(bp)
	})
	if avg > pooledEncodeBudget {
		t.Fatalf("pooled encode allocs/op = %.2f, budget %.2f", avg, pooledEncodeBudget)
	}
	t.Logf("pooled encode = %.2f allocs/op (budget %.2f)", avg, pooledEncodeBudget)
}

// TestBenchmarkLargeOrderEncodeHeapFreshBudget locks an empirically
// measured ceiling on the fresh-buffer encode path. The number reflects
// Go append-growth at the 1.5 MiB boundary; if the runtime's slice-grow
// strategy changes or the payload generator's encoded size shifts, this
// budget must be re-measured and re-locked, not loosened.
func TestBenchmarkLargeOrderEncodeHeapFreshBudget(t *testing.T) {
	if testing.Short() {
		t.Skip("alloc budget runs full")
	}
	m, _, bodyLen := newEncodeFixture(t)
	avg := testing.AllocsPerRun(20, func() {
		w := gsbm.NewWriter(nil)
		w.WriteHeader(0, 1, bodyLen)
		if err := m.MarshalGSBM(w); err != nil {
			t.Fatal(err)
		}
		_ = w.Bytes()
	})
	if avg > freshEncodeBudget {
		t.Fatalf("fresh encode allocs/op = %.2f, budget %.2f (see freshEncodeBudget comment)", avg, freshEncodeBudget)
	}
	t.Logf("fresh encode = %.2f allocs/op (budget %.2f)", avg, freshEncodeBudget)
}

// moneyBenchLines bounds the BenchmarkEncodeMoneyBatch payload — 1,000
// lines × 3 Money values per line (Base + one Tax + one Adjustment),
// mirroring the example in issue #29. Same payload feeds both
// sub-benchmarks; only the codec choice for Money.Amount differs.
const moneyBenchLines = 1000

// BenchmarkEncodeMoneyBatch contrasts the two materializing-decimal
// codec flavors on a Money-heavy payload. The String sub-benchmark
// encodes via EmitDecimalString; the Append sub-benchmark encodes via
// EmitDecimalAppend against the byte-identical wire output.
//
// Run as:
//
//	go test -bench=BenchmarkEncodeMoneyBatch -benchmem -benchtime=3s \
//	    ./storage/gsbm/
//
// allocs/op is the headline metric — the Append path skips the
// intermediate-string materialization entirely.
func BenchmarkEncodeMoneyBatch(b *testing.B) {
	batch := money.MakeBatch(0, moneyBenchLines)
	sb := money.StringBatchFrom(batch)
	ab := money.AppendBatchFrom(batch)

	b.Run("String", func(b *testing.B) {
		// Warm-up so per-Marshal one-shot initializations aren't charged.
		if _, err := gsbm.Marshal(&sb, 1); err != nil {
			b.Fatal(err)
		}
		b.ReportAllocs()
		for b.Loop() {
			if _, err := gsbm.Marshal(&sb, 1); err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("Append", func(b *testing.B) {
		if _, err := gsbm.Marshal(&ab, 1); err != nil {
			b.Fatal(err)
		}
		b.ReportAllocs()
		for b.Loop() {
			if _, err := gsbm.Marshal(&ab, 1); err != nil {
				b.Fatal(err)
			}
		}
	})
}

// peakBenchAttachments / peakBenchPayloadDataLen size the JSON-payload
// batch driven by BenchmarkEncodePeakMemoryStreamingVsCached. The
// per-payload Data slice grows under base64 encoding (≈ 4/3 inflation)
// inside json.Marshal, so 256 × 32 KiB raw lands at ~11 MiB of on-wire
// body bytes — large enough that the materializing-cached scratch
// (~1× wire blob) and the output buffer (~1× wire blob) are each many
// allocator size-class rounds, smoothing out HeapAlloc jitter that
// dominates smaller fixtures. CI memory cost: streaming peak ~14 MiB,
// cached peak ~24 MiB above baseline; comfortable inside the standard
// CI container budget.
const (
	peakBenchAttachments     = 256
	peakBenchPayloadDataLen  = 32 * 1024
	peakStreamingRatioMax    = 1.6  // streaming peak ≤ 1.6 × blob (issue #30 acceptance)
	peakCachedRatioMin       = 2.0  // cached peak ≥ 2.0 × blob (issue #30 acceptance)
	peakStreamingVsCachedMax = 0.70 // streaming peak ≤ 70 % of cached peak (≥ 30 % reduction)
)

// measurePeakDelta runs MarshalWithProbe against v and returns the max
// HeapAlloc delta over the Mid and Late samples, the wire blob length
// (for ratio normalization), and the diagnostic scratch / output byte
// counters captured at the end of the write pass. Mid is taken with
// the output buffer already allocated and the scratch already
// transferred onto the write-mode Writer, so it captures the peak for
// the materializing-cached path (scratch + freshly allocated output);
// Late catches the streaming path's peak (output growing while
// in-flight materializations are short-lived).
func measurePeakDelta(t testing.TB, v gsbm.Marshaler) (peak uint64, blobLen int, scratch, output uint64) {
	t.Helper()
	blob, s, err := gsbm.MarshalWithProbe(v, 1)
	if err != nil {
		t.Fatalf("MarshalWithProbe: %v", err)
	}
	mid := s.Mid.HeapAlloc - s.Before.HeapAlloc
	late := s.Late.HeapAlloc - s.Before.HeapAlloc
	peak = max(mid, late)
	return peak, len(blob), s.ScratchBytes, s.OutputBytes
}

// BenchmarkEncodePeakMemoryStreamingVsCached contrasts peak heap during
// gsbm.Marshal between the streaming and materializing-cached codec
// kinds (issue #30) on a JSON-payload-heavy batch. The two encode roots
// (largepayload.StreamingBatch, largepayload.CachedBatch) emit
// byte-identical wire output for the same input — only the runtime
// retention shape differs.
//
// Methodology: gsbm.MarshalWithProbe is a hand-rolled twin of
// gsbm.Marshal that takes synchronous-GC samples at the size→write
// transition (with the output buffer allocated and scratch transferred
// onto the write-mode Writer) and after the write pass returns. Peak
// = max(Mid, Late) − Before, normalized against the wire blob length.
//
// Acceptance criteria (plan §"Peak-memory benchmark"):
//   - streaming peak ≤ 1.6 × len(blob)
//   - cached peak ≥ 2.0 × len(blob)
//   - streaming peak ≤ 0.7 × cached peak (≥ 30 % reduction)
//
// Run as:
//
//	go test -bench=BenchmarkEncodePeakMemoryStreamingVsCached -benchmem -benchtime=3s \
//	    ./storage/gsbm/
//
// The benchmark itself reports the measured numbers; the partnered
// TestEncodePeakMemoryStreamingVsCachedBudget asserts the acceptance
// thresholds so a regression surfaces under `go test` without needing
// `-bench`.
func BenchmarkEncodePeakMemoryStreamingVsCached(b *testing.B) {
	atts := largepayload.MakeBatch(0, peakBenchAttachments, peakBenchPayloadDataLen)
	sb := largepayload.StreamingBatch{Attachments: atts}
	cb := largepayload.CachedBatch{Attachments: atts}

	// Wire-equality drift guard: a divergence between the two paths
	// would make the peak comparison meaningless.
	sBuf, err := gsbm.Marshal(&sb, 1)
	if err != nil {
		b.Fatalf("Marshal streaming: %v", err)
	}
	cBuf, err := gsbm.Marshal(&cb, 1)
	if err != nil {
		b.Fatalf("Marshal cached: %v", err)
	}
	if !bytes.Equal(sBuf, cBuf) {
		b.Fatalf("streaming and cached wire bytes diverge: len(stream)=%d len(cached)=%d", len(sBuf), len(cBuf))
	}

	streamPeak, streamBlob, _, streamOut := measurePeakDelta(b, &sb)
	cachedPeak, cachedBlob, cachedScratch, cachedOut := measurePeakDelta(b, &cb)
	b.Logf("blob bytes        = %d", streamBlob)
	b.Logf("streaming peak    = %d (%.2fx blob), output cap %d", streamPeak, float64(streamPeak)/float64(streamBlob), streamOut)
	b.Logf("cached peak       = %d (%.2fx blob), scratch %d, output cap %d", cachedPeak, float64(cachedPeak)/float64(cachedBlob), cachedScratch, cachedOut)
	if cachedPeak > 0 {
		b.Logf("streaming/cached  = %.2f (%.1f%% reduction)", float64(streamPeak)/float64(cachedPeak), 100*(1-float64(streamPeak)/float64(cachedPeak)))
	}

	// b.Loop exists so `go test -bench=` reports ns/op for the
	// streaming-encode path; the per-iteration cost stays comparable
	// to BenchmarkMarshalLargeOrder while the peak-heap signal lives
	// in the b.Logf lines above.
	if _, err := gsbm.Marshal(&sb, 1); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := gsbm.Marshal(&sb, 1); err != nil {
			b.Fatal(err)
		}
	}
}

// TestEncodePeakMemoryStreamingVsCachedBudget pins the issue-#30
// acceptance criteria as a regular test so a regression in either
// codec kind's peak-heap shape fails CI rather than waiting for a
// `-bench` invocation. Numbers and methodology mirror
// BenchmarkEncodePeakMemoryStreamingVsCached.
func TestEncodePeakMemoryStreamingVsCachedBudget(t *testing.T) {
	if testing.Short() {
		t.Skip("peak-memory probe runs full")
	}
	atts := largepayload.MakeBatch(0, peakBenchAttachments, peakBenchPayloadDataLen)
	sb := largepayload.StreamingBatch{Attachments: atts}
	cb := largepayload.CachedBatch{Attachments: atts}

	streamPeak, streamBlob, _, _ := measurePeakDelta(t, &sb)
	cachedPeak, cachedBlob, _, _ := measurePeakDelta(t, &cb)

	streamRatio := float64(streamPeak) / float64(streamBlob)
	cachedRatio := float64(cachedPeak) / float64(cachedBlob)
	relRatio := float64(streamPeak) / float64(cachedPeak)
	t.Logf("streaming peak / blob = %.2f (budget ≤ %.2f)", streamRatio, peakStreamingRatioMax)
	t.Logf("cached peak / blob    = %.2f (budget ≥ %.2f)", cachedRatio, peakCachedRatioMin)
	t.Logf("streaming / cached    = %.2f (budget ≤ %.2f, ≥ 30%% reduction)", relRatio, peakStreamingVsCachedMax)

	if streamRatio > peakStreamingRatioMax {
		t.Errorf("streaming peak %d > %.2f × blob %d (ratio %.2f)", streamPeak, peakStreamingRatioMax, streamBlob, streamRatio)
	}
	if cachedRatio < peakCachedRatioMin {
		t.Errorf("cached peak %d < %.2f × blob %d (ratio %.2f) — materializing-cached path no longer retains the scratch alongside the output; investigate before relaxing", cachedPeak, peakCachedRatioMin, cachedBlob, cachedRatio)
	}
	if relRatio > peakStreamingVsCachedMax {
		t.Errorf("streaming peak %d > %.2f × cached peak %d (ratio %.2f) — streaming-codec peak reduction has regressed below 30 %%", streamPeak, peakStreamingVsCachedMax, cachedPeak, relRatio)
	}
}

// TestMoneyBatchWireEquality is the drift guard for the
// BenchmarkEncodeMoneyBatch payload: the two codec paths must encode to
// byte-identical wire output for the full 1,000-line fixture, otherwise
// the alloc deltas measured by the benchmark are comparing apples to
// oranges.
func TestMoneyBatchWireEquality(t *testing.T) {
	batch := money.MakeBatch(0, moneyBenchLines)
	sb := money.StringBatchFrom(batch)
	ab := money.AppendBatchFrom(batch)
	bs, err := gsbm.Marshal(&sb, 1)
	if err != nil {
		t.Fatalf("Marshal String: %v", err)
	}
	ba, err := gsbm.Marshal(&ab, 1)
	if err != nil {
		t.Fatalf("Marshal Append: %v", err)
	}
	if !bytes.Equal(bs, ba) {
		t.Fatalf("wire bytes diverged: len(string)=%d len(append)=%d", len(bs), len(ba))
	}
}

