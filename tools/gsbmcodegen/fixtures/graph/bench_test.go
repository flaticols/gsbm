package graph_test

import (
	"sync"
	"testing"

	"go.flaticols.dev/gsbm/internal/bench"
	"go.flaticols.dev/gsbm/storage/gsbm"
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
