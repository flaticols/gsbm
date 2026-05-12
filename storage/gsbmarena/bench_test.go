package gsbmarena_test

import (
	"sync"
	"testing"

	"go.flaticols.dev/gsbm/internal/bench"
	"go.flaticols.dev/gsbm/storage/gsbm"
	"go.flaticols.dev/gsbm/storage/gsbmarena"
	"go.flaticols.dev/gsbm/tools/gsbmcodegen/fixtures/sample"
)

// largeOrderTargetMin / largeOrderTargetMax bracket the bench payload
// body size. Mirrors the values used in storage/gsbm/bench_*_test.go;
// kept duplicated rather than exporting from a shared internal because
// these are test-package constants and the bench fixture seed is shared.
const (
	largeOrderTargetMin = 1 << 20
	largeOrderTargetMax = 2 << 20
)

// largeOrderDecodeArenaShotBudget bounds the arena single-shot decode.
// Each iteration creates a fresh *gsbmarena.Arena, decodes the blob,
// and Releases. The arena bump-allocates string bytes into chunked
// buffers (4 KiB each, see arena.go:47) and pools per-T slice
// allocations, so the per-op count is dominated by:
//   - the *Arena alloc itself (1)
//   - one byte chunk per ~4 KiB of total decoded string bytes
//   - one slice-pool entry + chunk per distinct element type T
//   - one map per heap-allocated map field (Tags, Aliases) — maps stay
//     on the heap in Path A (see arena.go package doc)
//   - the slicePools map alloc (1)
//
// Locked from measured baseline (55097 allocs/op at seed=0, target
// range 1-2 MiB on Go 1.26) + ~27% slack. Arena chunks are
// proportional to total string-byte payload, so a different fixture
// size changes this number — re-measure if MakeLargeOrder is retuned.
const largeOrderDecodeArenaShotBudget = 70000.0

// largeOrderDecodeArenaPoolBudget bounds the pooled-arena variant.
// Pooling a *gsbmarena.Arena saves the *Arena heap alloc per op
// (negligible against chunk allocs); Release still drops the chunks
// and slice pools, since the arena lifetime contract requires it
// (arena.go:78-90 — references handed out by a prior decode become
// invalid after Release, so chunks cannot be carried across decodes
// without an arena-aware reset, which v1 does not provide). The
// budget is therefore close to the shot budget. Measured 44079
// allocs/op at seed=0 (lower than shot because the pre-warmed pool
// seeded the first iteration's typed-pool map outside AllocsPerRun's
// averaging window) + ~27% slack.
const largeOrderDecodeArenaPoolBudget = 56000.0

// newDecodeBlob produces a stable wire blob (header + body) for the
// 1-2 MiB Order using the same seed as storage/gsbm/bench_*.
func newDecodeBlob(tb testing.TB) []byte {
	tb.Helper()
	o := bench.MakeLargeOrder(0, largeOrderTargetMin, largeOrderTargetMax)
	bodySize, err := bench.EncodedSize(&o)
	if err != nil {
		tb.Fatalf("EncodedSize: %v", err)
	}
	w := gsbm.NewWriter(nil)
	w.WriteHeader(0, 1, uint32(bodySize))
	if err := o.MarshalGSBM(w); err != nil {
		tb.Fatalf("marshal: %v", err)
	}
	if err := w.Err(); err != nil {
		tb.Fatalf("writer err: %v", err)
	}
	return w.Bytes()
}

// BenchmarkLargeOrderDecodeArenaShot measures fresh-arena, single-shot
// decode: allocate, decode, Release. Even though the Order and its
// nested receivers are arena-allocated, sample.DecodeOrder routes
// through the heap-mode UnmarshalGSBM body (see order_gsbm_arena.go),
// which calls MarkPresent on every receiver. The package-level
// presence-track sidecar therefore accumulates one entry per nested
// receiver per iteration; drain it explicitly to keep AllocsPerRun
// deterministic across runs (see presence_track.go:137 and the
// FuzzArenaDecodeAgainstHeap pattern at arena_test.go:213).
func BenchmarkLargeOrderDecodeArenaShot(b *testing.B) {
	blob := newDecodeBlob(b)
	b.ReportAllocs()
	for b.Loop() {
		a := gsbmarena.NewArena()
		if _, err := sample.DecodeOrder(blob, a); err != nil {
			b.Fatal(err)
		}
		a.Release()
		// Drain the test-only sidecar outside the timed window so ns/op
		// and MB/s reflect decode cost, not the O(receivers) sync.Map
		// scan that production never performs.
		b.StopTimer()
		gsbm.ResetPresenceStore()
		b.StartTimer()
	}
}

// BenchmarkLargeOrderDecodeArenaPool measures the pooled-arena variant.
// Release is still called between iterations because the arena lifetime
// contract (arena.go package doc, "Lifetime contract") forbids reusing
// chunks across decodes without invalidating prior references. The
// pool-vs-shot delta is therefore small — the *Arena struct alloc is
// the only meaningful saving. The presence-track sidecar still grows
// per iteration (the structs allocated inside the arena are not
// pooled), so drain it for the same reason as the shot variant.
func BenchmarkLargeOrderDecodeArenaPool(b *testing.B) {
	blob := newDecodeBlob(b)
	pool := sync.Pool{New: func() any { return gsbmarena.NewArena() }}
	{
		a := pool.Get().(*gsbmarena.Arena)
		if _, err := sample.DecodeOrder(blob, a); err != nil {
			b.Fatal(err)
		}
		a.Release()
		pool.Put(a)
	}
	gsbm.ResetPresenceStore()
	b.ReportAllocs()
	for b.Loop() {
		a := pool.Get().(*gsbmarena.Arena)
		if _, err := sample.DecodeOrder(blob, a); err != nil {
			b.Fatal(err)
		}
		a.Release()
		pool.Put(a)
		// Sidecar drain is test-only bookkeeping; keep it outside the
		// timed window so the bench reports decode cost, not cleanup.
		b.StopTimer()
		gsbm.ResetPresenceStore()
		b.StartTimer()
	}
}

// TestBenchmarkLargeOrderDecodeArenaShotBudget locks the shot ceiling.
// Arena allocations are graph-size-relative (one chunk per ~4 KiB of
// decoded string bytes); a future change to MakeLargeOrder's size
// targets must re-measure this constant.
func TestBenchmarkLargeOrderDecodeArenaShotBudget(t *testing.T) {
	if testing.Short() {
		t.Skip("alloc budget runs full")
	}
	blob := newDecodeBlob(t)
	avg := testing.AllocsPerRun(20, func() {
		a := gsbmarena.NewArena()
		if _, err := sample.DecodeOrder(blob, a); err != nil {
			t.Fatal(err)
		}
		a.Release()
		gsbm.ResetPresenceStore()
	})
	if avg > largeOrderDecodeArenaShotBudget {
		t.Fatalf("arena shot allocs/op = %.2f, budget %.2f", avg, largeOrderDecodeArenaShotBudget)
	}
	t.Logf("arena shot = %.2f allocs/op (budget %.2f)", avg, largeOrderDecodeArenaShotBudget)
}

// TestBenchmarkLargeOrderDecodeArenaPoolBudget asserts pool reuse
// stays within budget. The expected delta from shot is small because
// Release drops byte chunks and slice pools (see comment on
// largeOrderDecodeArenaPoolBudget).
func TestBenchmarkLargeOrderDecodeArenaPoolBudget(t *testing.T) {
	if testing.Short() {
		t.Skip("alloc budget runs full")
	}
	blob := newDecodeBlob(t)
	pool := sync.Pool{New: func() any { return gsbmarena.NewArena() }}
	{
		a := pool.Get().(*gsbmarena.Arena)
		if _, err := sample.DecodeOrder(blob, a); err != nil {
			t.Fatal(err)
		}
		a.Release()
		pool.Put(a)
	}
	gsbm.ResetPresenceStore()
	avg := testing.AllocsPerRun(20, func() {
		a := pool.Get().(*gsbmarena.Arena)
		if _, err := sample.DecodeOrder(blob, a); err != nil {
			t.Fatal(err)
		}
		a.Release()
		pool.Put(a)
		gsbm.ResetPresenceStore()
	})
	if avg > largeOrderDecodeArenaPoolBudget {
		t.Fatalf("arena pool allocs/op = %.2f, budget %.2f", avg, largeOrderDecodeArenaPoolBudget)
	}
	t.Logf("arena pool = %.2f allocs/op (budget %.2f)", avg, largeOrderDecodeArenaPoolBudget)
}
