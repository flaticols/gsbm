package repeatednested_test

import (
	"fmt"
	"runtime"
	"testing"

	"go.flaticols.dev/gsbm/internal/bench/repeatednested"
	"go.flaticols.dev/gsbm/storage/gsbm"
)

// liveHeap forces two GC cycles (the second collects objects whose
// finalizers freed memory in the first) and reports the live heap.
func liveHeap() uint64 {
	runtime.GC()
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return ms.HeapAlloc
}

// decodeHeap and decodeInterned decode the same blob via the two paths.
func decodeHeap(tb testing.TB, blob []byte) *repeatednested.Batch {
	out := new(repeatednested.Batch)
	if err := gsbm.DecodeInto(blob, out); err != nil {
		tb.Fatal(err)
	}
	return out
}

func decodeInterned(tb testing.TB, blob []byte) *repeatednested.Batch {
	out := new(repeatednested.Batch)
	if err := gsbm.DecodeInterned(blob, out); err != nil {
		tb.Fatal(err)
	}
	return out
}

// TestInternedDedupWin quantifies the dedup: ~34k string occurrences in the
// N=1000 batch collapse to the distinct-value count (≈1080: 1000 unique
// IDs + the 80 pooled airport/currency/tax codes). The pooled strings —
// ~33k occurrences folded onto 80 backing arrays — are the resident win.
func TestInternedDedupWin(t *testing.T) {
	const n = 1000
	batch := repeatednested.MakeBatch(0, n, 5, 2)
	blob, err := gsbm.Marshal(&batch, 1)
	if err != nil {
		t.Fatal(err)
	}

	in := gsbm.NewInterner()
	out := new(repeatednested.Batch)
	r := gsbm.NewReader(blob)
	r.SetAllocator(in)
	if _, _, _, err := r.ReadHeader(); err != nil {
		t.Fatal(err)
	}
	if err := out.UnmarshalGSBM(r); err != nil {
		t.Fatal(err)
	}

	// Count string occurrences in the decoded graph.
	occ := 0
	for _, it := range out.Items {
		occ += 4 // ID, Origin, Dest, Currency
		for _, ln := range it.Lines {
			occ += 2 // Code, Money.Currency
			occ += 2 * len(ln.Taxes)
		}
	}
	distinct := in.Len()
	t.Logf("N=%d: %d string occurrences -> %d distinct (%.1fx fold)",
		n, occ, distinct, float64(occ)/float64(distinct))
	if distinct >= occ {
		t.Fatalf("no dedup: distinct=%d occurrences=%d", distinct, occ)
	}
	// The pooled codes (≈80 distinct) must have folded hard; distinct should
	// be dominated by the 1000 unique IDs, far below the occurrence count.
	if distinct > n+200 {
		t.Fatalf("dedup weaker than expected: distinct=%d, want <= %d", distinct, n+200)
	}
}

// retainedPerGraph holds K independently decoded graphs and reports the
// live-heap bytes attributable to one graph (transients GC'd, K graphs
// pinned). Averaging over K damps GC granularity noise.
func retainedPerGraph(decode func() *repeatednested.Batch) uint64 {
	const K = 48
	graphs := make([]*repeatednested.Batch, K)
	base := liveHeap()
	for i := range graphs {
		graphs[i] = decode()
	}
	after := liveHeap()
	runtime.KeepAlive(graphs)
	if after <= base {
		return 0
	}
	return (after - base) / K
}

// TestInternedRetainedGraphShrinks measures the resident graph: heap decode
// (every string copied) vs interned decode (duplicate backings folded). The
// interned graph must be meaningfully smaller on this repetition-heavy
// fixture. This is the "don't duplicate the same values" resident win.
func TestInternedRetainedGraphShrinks(t *testing.T) {
	const n = 1000
	batch := repeatednested.MakeBatch(0, n, 5, 2)
	blob, err := gsbm.Marshal(&batch, 1)
	if err != nil {
		t.Fatal(err)
	}

	heapBytes := retainedPerGraph(func() *repeatednested.Batch { return decodeHeap(t, blob) })
	internBytes := retainedPerGraph(func() *repeatednested.Batch { return decodeInterned(t, blob) })
	t.Logf("retained graph/decode: heap=%d KiB interned=%d KiB (%.1f%% of heap)",
		heapBytes/1024, internBytes/1024, 100*float64(internBytes)/float64(heapBytes))
	if internBytes == 0 || heapBytes == 0 {
		t.Skip("heap measurement underflowed (GC granularity) — rerun")
	}
	if internBytes >= heapBytes {
		t.Fatalf("interned graph not smaller: heap=%d interned=%d", heapBytes, internBytes)
	}
}

// BenchmarkDecodeRepeatedNested_Interned mirrors the existing
// uncompressed/zstd/gzip decode benches but with the interning allocator
// installed. Compared against BenchmarkDecodeRepeatedNested_Uncompressed it
// shows the per-string-copy allocations collapsing on a repeated-value
// payload (the win does NOT generalize to low-repetition payloads — see
// storage/gsbm TestInternedDecodeLowRepetitionNoCorruption).
func BenchmarkDecodeRepeatedNested_Interned(b *testing.B) {
	for _, n := range benchItemCounts {
		b.Run(fmt.Sprintf("N=%d", n), func(b *testing.B) {
			batch := repeatednested.MakeBatch(0, n, nDefaultLinesPerItem, nDefaultTaxesPerLine)
			blob, err := gsbm.Marshal(&batch, 1)
			if err != nil {
				b.Fatalf("Marshal: %v", err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				out := new(repeatednested.Batch)
				if err := gsbm.DecodeInterned(blob, out); err != nil {
					b.Fatalf("DecodeInterned: %v", err)
				}
			}
			b.StopTimer()
			b.ReportMetric(float64(len(blob)), "bytes/blob")
		})
	}
}
