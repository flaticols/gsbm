package gsbm_test

import (
	"sync"
	"testing"

	"go.flaticols.dev/gsbm/internal/bench"
	"go.flaticols.dev/gsbm/storage/gsbm"
	"go.flaticols.dev/gsbm/tools/gsbmcodegen/fixtures/sample"
)

// largeOrderDecodeColdBudget caps the heap-mode cold-decode allocation
// count for a 1-2 MiB MakeLargeOrder payload. Cold decode allocates one
// `string([]byte)` copy per ReadString plus one backing slice per
// repeated/map field per call, so the count scales with graph cardinality
// (Items, Tags, Aliases, QtyList, LabelList, Counts, payloads). After
// the local-bitmap migration (PR #24) the per-receiver MarkPresent +
// sync.Map sidecar allocations are gone; measured baseline 45240
// allocs/op at seed=0 on Go 1.26. Budget kept at the legacy 120000
// ceiling for now so this PR does not bundle a budget tightening; a
// follow-up may lower it once jitter across machines is characterised.
const largeOrderDecodeColdBudget = 120000.0

// largeOrderDecodeWarmBudget bounds warm-pool decode. With a sync.Pool
// of *sample.Order, Reset preserves slice/map capacity (per
// reset_test.go:TestResetPreservesCapacity) so subsequent decodes reuse
// the backing storage. The remaining floor is the per-string copy on
// every ReadString plus map-bucket allocs that ride with map grow.
// Measured baseline 45075 allocs/op + ~27% slack.
const largeOrderDecodeWarmBudget = 57000.0

// largeOrderRoundTripBudget is the round-trip (encode → decode) ceiling
// using a pooled []byte buffer for encode and a *sample.Order pool for
// decode. The dominant cost is decode (encode pooled is 3 allocs/op per
// pooledEncodeBudget); measured 45077 allocs/op at seed=0, locked at
// the same +27% slack as the warm-decode budget.
const largeOrderRoundTripBudget = 57000.0

// newDecodeBlob produces a stable wire blob for the 1-2 MiB Order. The
// blob includes the 12-byte header so DecodeInto can parse it. Seed=0
// keeps the byte sequence reproducible across runs.
func newDecodeBlob(tb testing.TB) []byte {
	tb.Helper()
	o := bench.MakeLargeOrder(0, largeOrderTargetMin, largeOrderTargetMax)
	// Marshal the body first to learn its byte count, then assemble a
	// full blob with the correct bodyLen baked into the header.
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

// BenchmarkLargeOrderDecodeHeapCold measures cold-path heap decode with
// no receiver pool. Each iteration allocates a fresh *sample.Order.
// Generated UnmarshalGSBM no longer writes to the package-level
// presence-track sidecar (it uses a stack-local bitmap that dies with
// the call), so this bench does not need to drain presenceStore between
// iterations.
func BenchmarkLargeOrderDecodeHeapCold(b *testing.B) {
	blob := newDecodeBlob(b)
	b.ReportAllocs()
	for b.Loop() {
		o := new(sample.Order)
		if err := gsbm.DecodeInto(blob, o); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkLargeOrderDecodeHeapWarm measures the production hot path:
// a sync.Pool of *sample.Order, gsbm.DecodeInto. Reset preserves slice
// and map capacity so steady-state allocations are dominated by per-
// string copies (every ReadString allocates) plus the pool-Get
// interface boxing.
func BenchmarkLargeOrderDecodeHeapWarm(b *testing.B) {
	blob := newDecodeBlob(b)
	pool := sync.Pool{New: func() any { return new(sample.Order) }}
	// Pre-warm the pool so slice/map capacities are seeded from a real
	// decode; without warm-up the first iteration of the bench dominates
	// the average and bench numbers misrepresent steady-state.
	{
		o := pool.Get().(*sample.Order)
		if err := gsbm.DecodeInto(blob, o); err != nil {
			b.Fatal(err)
		}
		pool.Put(o)
	}
	b.ReportAllocs()
	for b.Loop() {
		o := pool.Get().(*sample.Order)
		if err := gsbm.DecodeInto(blob, o); err != nil {
			b.Fatal(err)
		}
		pool.Put(o)
	}
}

// BenchmarkLargeOrderRoundTrip exercises encode-then-decode in a single
// op with both the encode buffer and the decode receiver drawn from
// pools. Approximates the close-loop cost of a server emitting a blob
// and a downstream consumer parsing it.
func BenchmarkLargeOrderRoundTrip(b *testing.B) {
	o := bench.MakeLargeOrder(0, largeOrderTargetMin, largeOrderTargetMax)
	bodySize, err := bench.EncodedSize(&o)
	if err != nil {
		b.Fatalf("EncodedSize: %v", err)
	}
	bodyLen := uint32(bodySize)
	bufPool := sync.Pool{New: func() any {
		buf := make([]byte, 0, largeOrderTargetMax+64)
		return &buf
	}}
	dstPool := sync.Pool{New: func() any { return new(sample.Order) }}
	{
		bp := bufPool.Get().(*[]byte)
		w := gsbm.NewWriter((*bp)[:0])
		w.WriteHeader(0, 1, bodyLen)
		if err := o.MarshalGSBM(w); err != nil {
			b.Fatal(err)
		}
		blob := w.Bytes()
		dst := dstPool.Get().(*sample.Order)
		if err := gsbm.DecodeInto(blob, dst); err != nil {
			b.Fatal(err)
		}
		dstPool.Put(dst)
		*bp = blob
		bufPool.Put(bp)
	}
	b.ReportAllocs()
	for b.Loop() {
		bp := bufPool.Get().(*[]byte)
		w := gsbm.NewWriter((*bp)[:0])
		w.WriteHeader(0, 1, bodyLen)
		if err := o.MarshalGSBM(w); err != nil {
			b.Fatal(err)
		}
		blob := w.Bytes()
		dst := dstPool.Get().(*sample.Order)
		if err := gsbm.DecodeInto(blob, dst); err != nil {
			b.Fatal(err)
		}
		dstPool.Put(dst)
		*bp = blob
		bufPool.Put(bp)
	}
}

// TestBenchmarkLargeOrderDecodeHeapColdBudget locks the cold-decode
// allocation ceiling. The bound is graph-size-relative: doubling the
// fixture's collection cardinalities will roughly double the allocs and
// require a re-measure with the new size as the source of truth.
func TestBenchmarkLargeOrderDecodeHeapColdBudget(t *testing.T) {
	if testing.Short() {
		t.Skip("alloc budget runs full")
	}
	blob := newDecodeBlob(t)
	avg := testing.AllocsPerRun(20, func() {
		o := new(sample.Order)
		if err := gsbm.DecodeInto(blob, o); err != nil {
			t.Fatal(err)
		}
	})
	if avg > largeOrderDecodeColdBudget {
		t.Fatalf("cold heap decode allocs/op = %.2f, budget %.2f (re-measure if MakeLargeOrder size changed)", avg, largeOrderDecodeColdBudget)
	}
	t.Logf("cold heap decode = %.2f allocs/op (budget %.2f)", avg, largeOrderDecodeColdBudget)
}

// TestBenchmarkLargeOrderDecodeHeapWarmBudget asserts that pool reuse
// drops the alloc count well below cold. The string-copy floor is
// unavoidable on the heap path (see reset_test.go:147 stringAllocFloor
// rationale); the budget accounts for it plus benign jitter.
func TestBenchmarkLargeOrderDecodeHeapWarmBudget(t *testing.T) {
	if testing.Short() {
		t.Skip("alloc budget runs full")
	}
	blob := newDecodeBlob(t)
	pool := sync.Pool{New: func() any { return new(sample.Order) }}
	{
		o := pool.Get().(*sample.Order)
		if err := gsbm.DecodeInto(blob, o); err != nil {
			t.Fatal(err)
		}
		pool.Put(o)
	}
	avg := testing.AllocsPerRun(20, func() {
		o := pool.Get().(*sample.Order)
		if err := gsbm.DecodeInto(blob, o); err != nil {
			t.Fatal(err)
		}
		pool.Put(o)
	})
	if avg > largeOrderDecodeWarmBudget {
		t.Fatalf("warm heap decode allocs/op = %.2f, budget %.2f", avg, largeOrderDecodeWarmBudget)
	}
	t.Logf("warm heap decode = %.2f allocs/op (budget %.2f)", avg, largeOrderDecodeWarmBudget)
}

// TestBenchmarkLargeOrderRoundTripBudget asserts the round-trip stays
// within the sum of pooledEncodeBudget + largeOrderDecodeWarmBudget
// plus a small slack for the dst pool-Get boxing.
func TestBenchmarkLargeOrderRoundTripBudget(t *testing.T) {
	if testing.Short() {
		t.Skip("alloc budget runs full")
	}
	o := bench.MakeLargeOrder(0, largeOrderTargetMin, largeOrderTargetMax)
	bodySize, err := bench.EncodedSize(&o)
	if err != nil {
		t.Fatalf("EncodedSize: %v", err)
	}
	bodyLen := uint32(bodySize)
	bufPool := sync.Pool{New: func() any {
		buf := make([]byte, 0, largeOrderTargetMax+64)
		return &buf
	}}
	dstPool := sync.Pool{New: func() any { return new(sample.Order) }}
	{
		bp := bufPool.Get().(*[]byte)
		w := gsbm.NewWriter((*bp)[:0])
		w.WriteHeader(0, 1, bodyLen)
		if err := o.MarshalGSBM(w); err != nil {
			t.Fatal(err)
		}
		blob := w.Bytes()
		dst := dstPool.Get().(*sample.Order)
		if err := gsbm.DecodeInto(blob, dst); err != nil {
			t.Fatal(err)
		}
		dstPool.Put(dst)
		*bp = blob
		bufPool.Put(bp)
	}
	avg := testing.AllocsPerRun(20, func() {
		bp := bufPool.Get().(*[]byte)
		w := gsbm.NewWriter((*bp)[:0])
		w.WriteHeader(0, 1, bodyLen)
		if err := o.MarshalGSBM(w); err != nil {
			t.Fatal(err)
		}
		blob := w.Bytes()
		dst := dstPool.Get().(*sample.Order)
		if err := gsbm.DecodeInto(blob, dst); err != nil {
			t.Fatal(err)
		}
		dstPool.Put(dst)
		*bp = blob
		bufPool.Put(bp)
	})
	if avg > largeOrderRoundTripBudget {
		t.Fatalf("round-trip allocs/op = %.2f, budget %.2f", avg, largeOrderRoundTripBudget)
	}
	t.Logf("round-trip = %.2f allocs/op (budget %.2f)", avg, largeOrderRoundTripBudget)
}
