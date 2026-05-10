package graph_test

import (
	"sync"
	"testing"

	"go.flaticols.dev/gsbm/internal/bench"
	"go.flaticols.dev/gsbm/storage/gsbm"
	"go.flaticols.dev/gsbm/storage/gsbmarena"
	"go.flaticols.dev/gsbm/tools/gsbmcodegen/fixtures/graph"
)

const (
	largeCatalogTargetMin = 1 << 20
	largeCatalogTargetMax = 2 << 20
)

// pooledCatalogEncodeBudget caps the warm-pool encode allocation count
// for the graph fixture. Catalog has one map field (Tags) and emits one
// `make([]string, 0, len(m))` for sorted-key wire output (spec §5.3) on
// top of the *gsbm.Writer alloc; Sections is a slice and adds no
// per-encode alloc beyond inner sort buffers (Items has no map). The
// steady-state count is 2; 4 leaves headroom for benign jitter.
const pooledCatalogEncodeBudget = 4.0

func newCatalogFixture(tb testing.TB) bench.Marshaler {
	tb.Helper()
	c := bench.MakeLargeCatalog(0, largeCatalogTargetMin, largeCatalogTargetMax)
	m := bench.Marshaler(&c)
	size, err := bench.EncodedSize(m)
	if err != nil {
		tb.Fatalf("EncodedSize: %v", err)
	}
	if size < largeCatalogTargetMin || size > largeCatalogTargetMax {
		tb.Fatalf("MakeLargeCatalog size %d out of range [%d, %d]", size, largeCatalogTargetMin, largeCatalogTargetMax)
	}
	return m
}

func BenchmarkLargeCatalogEncodeHeapPooled(b *testing.B) {
	m := newCatalogFixture(b)
	pool := sync.Pool{New: func() any {
		buf := make([]byte, 0, largeCatalogTargetMax+64)
		return &buf
	}}
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

// TestBenchmarkLargeCatalogEncodeHeapPooledBudget asserts the pooled
// encode of a 1-2 MiB Catalog stays within the per-map sort-keys floor
// plus the *Writer alloc plus a small slack. See
// pooledCatalogEncodeBudget for the derivation.
func TestBenchmarkLargeCatalogEncodeHeapPooledBudget(t *testing.T) {
	if testing.Short() {
		t.Skip("alloc budget runs full")
	}
	m := newCatalogFixture(t)
	pool := sync.Pool{New: func() any {
		buf := make([]byte, 0, largeCatalogTargetMax+64)
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
	if avg > pooledCatalogEncodeBudget {
		t.Fatalf("catalog pooled encode allocs/op = %.2f, budget %.2f", avg, pooledCatalogEncodeBudget)
	}
	t.Logf("catalog pooled encode = %.2f allocs/op (budget %.2f)", avg, pooledCatalogEncodeBudget)
}

// largeCatalogDecodeHeapBudget caps cold heap decode for the graph
// fixture. Catalog has Sections (slice of Section) where each Section
// has Items (slice of Item with optional Note *string), plus a Tags
// map (string → Tag with optional Weight *int64). Allocations scale
// with sectionCount × itemsPerSection plus tagCount, dominated by
// per-string copies and the per-Item Note pointer-to-string allocations
// on the optional-in-slice path. Measured 384526 allocs/op at seed=0
// (target 1-2 MiB) on Go 1.26 + ~27% slack. The bench drains the
// package-level presence-track sidecar each iteration via
// gsbm.ResetPresenceStore so the measurement is deterministic
// regardless of allocator address reuse.
const largeCatalogDecodeHeapBudget = 490000.0

// largeCatalogDecodeArenaBudget caps arena decode for the graph fixture.
// The optional-in-slice path (Item.Note) and optional-in-map path
// (Tag.Weight) each allocate one heap-side pointer per non-nil entry,
// since arena AllocStruct returns pointers into pooled chunks but the
// codegen still emits *T fields with heap addresses for optional values
// — see graph/item_gsbm.go for the pattern. Measured 224505 allocs/op
// at seed=0 (target 1-2 MiB) on Go 1.26 + ~27% slack.
const largeCatalogDecodeArenaBudget = 285000.0

// newCatalogBlob produces the wire form of MakeLargeCatalog (with header).
func newCatalogBlob(tb testing.TB) []byte {
	tb.Helper()
	c := bench.MakeLargeCatalog(0, largeCatalogTargetMin, largeCatalogTargetMax)
	w := gsbm.NewWriter(nil)
	w.WriteHeader(0, 1)
	if err := c.MarshalGSBM(w); err != nil {
		tb.Fatalf("marshal: %v", err)
	}
	if err := w.Err(); err != nil {
		tb.Fatalf("writer err: %v", err)
	}
	return w.Bytes()
}

// BenchmarkLargeCatalogDecodeHeap measures cold heap decode of the
// graph fixture, exercising the optional-in-slice (Item.Note) and
// optional-in-map (Tag.Weight) paths. The package-level presence-track
// sidecar is drained per-iteration: ForgetPresence(c) would evict only
// the root, but the generated decoders MarkPresent on every nested
// *Section / *Item / *Tag receiver (see catalog_gsbm.go, section_gsbm.go,
// item_gsbm.go, tag_gsbm.go), so a full drain is needed to keep alloc
// counts deterministic across iterations.
func BenchmarkLargeCatalogDecodeHeap(b *testing.B) {
	blob := newCatalogBlob(b)
	b.ReportAllocs()
	for b.Loop() {
		c := new(graph.Catalog)
		if err := gsbm.DecodeInto(blob, c); err != nil {
			b.Fatal(err)
		}
		// Sidecar drain is test-only bookkeeping; keep it outside the
		// timed window so the bench reports decode cost, not cleanup.
		b.StopTimer()
		gsbm.ResetPresenceStore()
		b.StartTimer()
	}
}

// BenchmarkLargeCatalogDecodeArena measures arena single-shot decode
// of the graph fixture. Even though the structs are arena-allocated,
// DecodeCatalog routes through the heap-mode UnmarshalGSBM (see
// catalog_gsbm_arena.go) which calls MarkPresent on every nested
// receiver, so the package-level sidecar still accumulates entries.
// Drain it per iteration for the same determinism reason as the heap
// bench.
func BenchmarkLargeCatalogDecodeArena(b *testing.B) {
	blob := newCatalogBlob(b)
	b.ReportAllocs()
	for b.Loop() {
		a := gsbmarena.NewArena()
		if _, err := graph.DecodeCatalog(blob, a); err != nil {
			b.Fatal(err)
		}
		a.Release()
		// Sidecar drain is test-only bookkeeping; keep it outside the
		// timed window so the bench reports decode cost, not cleanup.
		b.StopTimer()
		gsbm.ResetPresenceStore()
		b.StartTimer()
	}
}

// TestBenchmarkLargeCatalogDecodeHeapBudget asserts the cold-heap
// decode of the graph fixture stays within budget.
func TestBenchmarkLargeCatalogDecodeHeapBudget(t *testing.T) {
	if testing.Short() {
		t.Skip("alloc budget runs full")
	}
	blob := newCatalogBlob(t)
	avg := testing.AllocsPerRun(20, func() {
		c := new(graph.Catalog)
		if err := gsbm.DecodeInto(blob, c); err != nil {
			t.Fatal(err)
		}
		gsbm.ResetPresenceStore()
	})
	if avg > largeCatalogDecodeHeapBudget {
		t.Fatalf("catalog heap decode allocs/op = %.2f, budget %.2f", avg, largeCatalogDecodeHeapBudget)
	}
	t.Logf("catalog heap decode = %.2f allocs/op (budget %.2f)", avg, largeCatalogDecodeHeapBudget)
}

// TestBenchmarkLargeCatalogDecodeArenaBudget asserts the arena decode
// of the graph fixture stays within budget.
func TestBenchmarkLargeCatalogDecodeArenaBudget(t *testing.T) {
	if testing.Short() {
		t.Skip("alloc budget runs full")
	}
	blob := newCatalogBlob(t)
	avg := testing.AllocsPerRun(20, func() {
		a := gsbmarena.NewArena()
		if _, err := graph.DecodeCatalog(blob, a); err != nil {
			t.Fatal(err)
		}
		a.Release()
		gsbm.ResetPresenceStore()
	})
	if avg > largeCatalogDecodeArenaBudget {
		t.Fatalf("catalog arena decode allocs/op = %.2f, budget %.2f", avg, largeCatalogDecodeArenaBudget)
	}
	t.Logf("catalog arena decode = %.2f allocs/op (budget %.2f)", avg, largeCatalogDecodeArenaBudget)
}
