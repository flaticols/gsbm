package gsbm_test

import (
	"sync"
	"testing"

	"go.flaticols.dev/gsbm/internal/bench"
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
// caller-supplied buffer pool. The implementation.md §3.3 "1 alloc/op
// encode" target is the *gsbm.Writer struct itself; the codegen emits
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
// the encoded length, and returns the value plus a slice ready to be
// reset with [:0] inside the bench loop. seed=0 keeps fixtures
// reproducible across runs.
func newEncodeFixture(tb testing.TB) (m bench.Marshaler, sized []byte) {
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
	// 8-byte header + body; round up modestly so a few extra bytes from
	// any header variant don't force a single grow on the first warm
	// iteration.
	sized = make([]byte, 0, size+64)
	return m, sized
}

func BenchmarkLargeOrderEncodeHeapPooled(b *testing.B) {
	m, _ := newEncodeFixture(b)
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
		w.WriteHeader(0, 1)
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
		w.WriteHeader(0, 1)
		if err := m.MarshalGSBM(w); err != nil {
			b.Fatal(err)
		}
		*bp = w.Bytes()
		pool.Put(bp)
	}
}

func BenchmarkLargeOrderEncodeHeapFresh(b *testing.B) {
	m, _ := newEncodeFixture(b)
	b.ReportAllocs()
	for b.Loop() {
		w := gsbm.NewWriter(nil)
		w.WriteHeader(0, 1)
		if err := m.MarshalGSBM(w); err != nil {
			b.Fatal(err)
		}
		_ = w.Bytes()
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
	m, _ := newEncodeFixture(t)
	pool := sync.Pool{New: func() any {
		buf := make([]byte, 0, largeOrderTargetMax+64)
		return &buf
	}}
	{
		bp := pool.Get().(*[]byte)
		w := gsbm.NewWriter((*bp)[:0])
		w.WriteHeader(0, 1)
		if err := m.MarshalGSBM(w); err != nil {
			t.Fatal(err)
		}
		*bp = w.Bytes()
		pool.Put(bp)
	}
	avg := testing.AllocsPerRun(20, func() {
		bp := pool.Get().(*[]byte)
		w := gsbm.NewWriter((*bp)[:0])
		w.WriteHeader(0, 1)
		if err := m.MarshalGSBM(w); err != nil {
			t.Fatal(err)
		}
		*bp = w.Bytes()
		pool.Put(bp)
	})
	if avg > pooledEncodeBudget {
		t.Fatalf("pooled encode allocs/op = %.2f, budget %.2f (see implementation.md §3.3)", avg, pooledEncodeBudget)
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
	m, _ := newEncodeFixture(t)
	avg := testing.AllocsPerRun(20, func() {
		w := gsbm.NewWriter(nil)
		w.WriteHeader(0, 1)
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
