package repeatednested_test

import (
	"fmt"
	"io"
	"testing"

	"go.flaticols.dev/gsbm/internal/bench/repeatednested"
	"go.flaticols.dev/gsbm/storage/gsbm"
)

// nDefaultLinesPerItem and nDefaultTaxesPerLine pin the per-item nesting
// shape for the parameter sweep. Only nItems varies across sub-bench
// dimensions so the recorded bytes/op trace cleanly attributes growth to
// item-count rather than confounded per-item fanout.
const (
	nDefaultLinesPerItem = 5
	nDefaultTaxesPerLine = 2

	// NestedBatch per-item nesting cardinality. Three parallel nested
	// slices per item mirrors the issue #58 shape (Legs / Prices / Tags
	// each contributing length-prefix bookkeeping). Only nItems varies
	// across the sweep so allocation deltas attribute cleanly to outer
	// fan-out, not per-item shape.
	nDefaultLegsPerItem   = 3
	nDefaultPricesPerItem = 4
	nDefaultTagsPerItem   = 5
)

var benchItemCounts = []int{10, 100, 1000}

// TestRoundTrip is the wire-correctness guard: encode the fixture, decode
// it, assert structural equality on a sample item. The bench is useless
// if the codec is wrong, so pin this before measuring anything.
func TestRoundTrip(t *testing.T) {
	in := repeatednested.MakeBatch(0, 8, nDefaultLinesPerItem, nDefaultTaxesPerLine)
	blob, err := gsbm.Marshal(&in, 1)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	r := gsbm.NewReader(blob)
	if _, _, _, err := r.ReadHeader(); err != nil {
		t.Fatalf("ReadHeader: %v", err)
	}
	var out repeatednested.Batch
	if err := out.UnmarshalGSBM(r); err != nil {
		t.Fatalf("UnmarshalGSBM: %v", err)
	}
	if len(out.Items) != len(in.Items) {
		t.Fatalf("Items len: got %d, want %d", len(out.Items), len(in.Items))
	}
	for i := range in.Items {
		if out.Items[i].ID != in.Items[i].ID {
			t.Fatalf("Items[%d].ID: got %q, want %q", i, out.Items[i].ID, in.Items[i].ID)
		}
		if out.Items[i].Origin != in.Items[i].Origin {
			t.Fatalf("Items[%d].Origin: got %q, want %q", i, out.Items[i].Origin, in.Items[i].Origin)
		}
		if len(out.Items[i].Lines) != len(in.Items[i].Lines) {
			t.Fatalf("Items[%d].Lines len: got %d, want %d", i, len(out.Items[i].Lines), len(in.Items[i].Lines))
		}
	}
}

// TestRoundTripNested is the wire-correctness guard for the
// nested-slice-heavy NestedBatch fixture added for issue #58. Each
// NestedItem carries three parallel nested slices (Legs, Prices, Tags)
// — the shape the bench measures. Round-trip equality on every field of
// every element pins that the new fixture's generated codec is correct
// before any allocation-profile measurement is taken.
func TestRoundTripNested(t *testing.T) {
	in := repeatednested.MakeNestedBatch(0, 8, 3, 4, 5)
	blob, err := gsbm.Marshal(&in, 1)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	r := gsbm.NewReader(blob)
	if _, _, _, err := r.ReadHeader(); err != nil {
		t.Fatalf("ReadHeader: %v", err)
	}
	var out repeatednested.NestedBatch
	if err := out.UnmarshalGSBM(r); err != nil {
		t.Fatalf("UnmarshalGSBM: %v", err)
	}
	if len(out.Items) != len(in.Items) {
		t.Fatalf("Items len: got %d, want %d", len(out.Items), len(in.Items))
	}
	for i := range in.Items {
		if out.Items[i].ID != in.Items[i].ID {
			t.Fatalf("Items[%d].ID: got %q, want %q", i, out.Items[i].ID, in.Items[i].ID)
		}
		if len(out.Items[i].Legs) != len(in.Items[i].Legs) {
			t.Fatalf("Items[%d].Legs len: got %d, want %d", i, len(out.Items[i].Legs), len(in.Items[i].Legs))
		}
		for j := range in.Items[i].Legs {
			if out.Items[i].Legs[j] != in.Items[i].Legs[j] {
				t.Fatalf("Items[%d].Legs[%d]: got %+v, want %+v", i, j, out.Items[i].Legs[j], in.Items[i].Legs[j])
			}
		}
		if len(out.Items[i].Prices) != len(in.Items[i].Prices) {
			t.Fatalf("Items[%d].Prices len: got %d, want %d", i, len(out.Items[i].Prices), len(in.Items[i].Prices))
		}
		for j := range in.Items[i].Prices {
			if out.Items[i].Prices[j] != in.Items[i].Prices[j] {
				t.Fatalf("Items[%d].Prices[%d]: got %+v, want %+v", i, j, out.Items[i].Prices[j], in.Items[i].Prices[j])
			}
		}
		if len(out.Items[i].Tags) != len(in.Items[i].Tags) {
			t.Fatalf("Items[%d].Tags len: got %d, want %d", i, len(out.Items[i].Tags), len(in.Items[i].Tags))
		}
		for j := range in.Items[i].Tags {
			if out.Items[i].Tags[j] != in.Items[i].Tags[j] {
				t.Fatalf("Items[%d].Tags[%d]: got %+v, want %+v", i, j, out.Items[i].Tags[j], in.Items[i].Tags[j])
			}
		}
	}
}

// TestMakeNestedBatchDeterministic confirms the generator is
// reproducible — same arguments yield byte-identical wire output, which
// the bench depends on for stable bytes/blob numbers.
func TestMakeNestedBatchDeterministic(t *testing.T) {
	a := repeatednested.MakeNestedBatch(7, 4, 2, 3, 2)
	b := repeatednested.MakeNestedBatch(7, 4, 2, 3, 2)
	ab, err := gsbm.Marshal(&a, 1)
	if err != nil {
		t.Fatalf("Marshal(a): %v", err)
	}
	bb, err := gsbm.Marshal(&b, 1)
	if err != nil {
		t.Fatalf("Marshal(b): %v", err)
	}
	if string(ab) != string(bb) {
		t.Fatalf("non-deterministic: len(a)=%d len(b)=%d", len(ab), len(bb))
	}
}

// BenchmarkEncodeRepeatedNested_Uncompressed reports bytes/op (encoded
// blob size) and allocs/op for today's uncompressed encode path across
// the parameter sweep. These numbers are the baseline the compressed
// path (added in a later task) will be compared against — recorded in
// the PR description as the "current size gap" datum the ticket calls
// for.
func BenchmarkEncodeRepeatedNested_Uncompressed(b *testing.B) {
	for _, n := range benchItemCounts {
		b.Run(fmt.Sprintf("N=%d", n), func(b *testing.B) {
			batch := repeatednested.MakeBatch(0, n, nDefaultLinesPerItem, nDefaultTaxesPerLine)
			// Prime once so size/allocs are stable from the first measured iteration.
			blob, err := gsbm.Marshal(&batch, 1)
			if err != nil {
				b.Fatalf("Marshal: %v", err)
			}
			blobLen := len(blob)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				out, err := gsbm.Marshal(&batch, 1)
				if err != nil {
					b.Fatalf("Marshal: %v", err)
				}
				_ = out
			}
			b.StopTimer()
			b.ReportMetric(float64(blobLen), "bytes/blob")
		})
	}
}

// BenchmarkDecodeRepeatedNested_Uncompressed mirrors the encode bench —
// reports allocs/op for the decode path so the compressed-decode path
// added later can be compared on the same axes.
func BenchmarkDecodeRepeatedNested_Uncompressed(b *testing.B) {
	for _, n := range benchItemCounts {
		b.Run(fmt.Sprintf("N=%d", n), func(b *testing.B) {
			batch := repeatednested.MakeBatch(0, n, nDefaultLinesPerItem, nDefaultTaxesPerLine)
			blob, err := gsbm.Marshal(&batch, 1)
			if err != nil {
				b.Fatalf("Marshal: %v", err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				r := gsbm.NewReader(blob)
				if _, _, _, err := r.ReadHeader(); err != nil {
					b.Fatalf("ReadHeader: %v", err)
				}
				var out repeatednested.Batch
				if err := out.UnmarshalGSBM(r); err != nil {
					b.Fatalf("UnmarshalGSBM: %v", err)
				}
			}
			b.StopTimer()
			b.ReportMetric(float64(len(blob)), "bytes/blob")
		})
	}
}

// BenchmarkEncodeRepeatedNested_Zstd is the compressed-encode mirror of
// BenchmarkEncodeRepeatedNested_Uncompressed. Reports compressed
// bytes/blob so the ratio against the uncompressed bench at each N is
// directly readable from the side-by-side output (the core hypothesis
// from the ticket is that this ratio improves as N grows).
func BenchmarkEncodeRepeatedNested_Zstd(b *testing.B) {
	for _, n := range benchItemCounts {
		b.Run(fmt.Sprintf("N=%d", n), func(b *testing.B) {
			batch := repeatednested.MakeBatch(0, n, nDefaultLinesPerItem, nDefaultTaxesPerLine)
			blob, err := gsbm.MarshalWithOptions(&batch, 1, gsbm.Options{Compress: true})
			if err != nil {
				b.Fatalf("MarshalWithOptions: %v", err)
			}
			blobLen := len(blob)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				out, err := gsbm.MarshalWithOptions(&batch, 1, gsbm.Options{Compress: true})
				if err != nil {
					b.Fatalf("MarshalWithOptions: %v", err)
				}
				_ = out
			}
			b.StopTimer()
			b.ReportMetric(float64(blobLen), "bytes/blob")
		})
	}
}

// BenchmarkDecodeRepeatedNested_Zstd mirrors the uncompressed-decode
// bench against a zstd-bodied blob — exercises the reader-side
// auto-detect + decompress path under the same parameter sweep.
func BenchmarkDecodeRepeatedNested_Zstd(b *testing.B) {
	for _, n := range benchItemCounts {
		b.Run(fmt.Sprintf("N=%d", n), func(b *testing.B) {
			batch := repeatednested.MakeBatch(0, n, nDefaultLinesPerItem, nDefaultTaxesPerLine)
			blob, err := gsbm.MarshalWithOptions(&batch, 1, gsbm.Options{Compress: true})
			if err != nil {
				b.Fatalf("MarshalWithOptions: %v", err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				r := gsbm.NewReader(blob)
				if _, _, _, err := r.ReadHeader(); err != nil {
					b.Fatalf("ReadHeader: %v", err)
				}
				var out repeatednested.Batch
				if err := out.UnmarshalGSBM(r); err != nil {
					b.Fatalf("UnmarshalGSBM: %v", err)
				}
			}
			b.StopTimer()
			b.ReportMetric(float64(len(blob)), "bytes/blob")
		})
	}
}

// BenchmarkEncodeRepeatedNestedStreaming_Zstd exercises the
// MarshalToWriter streaming path with compression enabled. Output goes
// to io.Discard so the measurement isolates encode cost (compression +
// frame writes) from any downstream consumer. Compared against
// BenchmarkEncodeRepeatedNested_Zstd this surfaces the streaming
// path's allocation profile vs the buffered MarshalWithOptions path.
func BenchmarkEncodeRepeatedNestedStreaming_Zstd(b *testing.B) {
	for _, n := range benchItemCounts {
		b.Run(fmt.Sprintf("N=%d", n), func(b *testing.B) {
			batch := repeatednested.MakeBatch(0, n, nDefaultLinesPerItem, nDefaultTaxesPerLine)
			// Capture compressed size once via the buffered path so the
			// streaming bench can report the same bytes/blob axis as the
			// other compressed benches — io.Discard hides it otherwise.
			pinned, err := gsbm.MarshalWithOptions(&batch, 1, gsbm.Options{Compress: true})
			if err != nil {
				b.Fatalf("MarshalWithOptions (pin): %v", err)
			}
			blobLen := len(pinned)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if err := gsbm.MarshalToWriter(io.Discard, &batch, 1, gsbm.Options{Compress: true}); err != nil {
					b.Fatalf("MarshalToWriter: %v", err)
				}
			}
			b.StopTimer()
			b.ReportMetric(float64(blobLen), "bytes/blob")
		})
	}
}

// BenchmarkEncodeNestedBatch_Uncompressed measures the buffered encode
// path on the issue #58 nested-slice-heavy shape (three parallel nested
// slices per item). Reports allocs/op and bytes/blob across the
// parameter sweep — the analytic-SizeGSBM + WriteLength codegen change
// targets the length-prefix bookkeeping that dominated this fixture's
// allocation profile pre-PR.
func BenchmarkEncodeNestedBatch_Uncompressed(b *testing.B) {
	for _, n := range benchItemCounts {
		b.Run(fmt.Sprintf("N=%d", n), func(b *testing.B) {
			batch := repeatednested.MakeNestedBatch(0, n, nDefaultLegsPerItem, nDefaultPricesPerItem, nDefaultTagsPerItem)
			blob, err := gsbm.Marshal(&batch, 1)
			if err != nil {
				b.Fatalf("Marshal: %v", err)
			}
			blobLen := len(blob)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				out, err := gsbm.Marshal(&batch, 1)
				if err != nil {
					b.Fatalf("Marshal: %v", err)
				}
				_ = out
			}
			b.StopTimer()
			b.ReportMetric(float64(blobLen), "bytes/blob")
		})
	}
}

// BenchmarkEncodeNestedBatch_Zstd is the buffered compressed-encode
// mirror of BenchmarkEncodeNestedBatch_Uncompressed. The compressed
// bytes/blob is captured once and reported as a metric so the
// compression ratio at each N is directly readable next to the
// uncompressed bench.
func BenchmarkEncodeNestedBatch_Zstd(b *testing.B) {
	for _, n := range benchItemCounts {
		b.Run(fmt.Sprintf("N=%d", n), func(b *testing.B) {
			batch := repeatednested.MakeNestedBatch(0, n, nDefaultLegsPerItem, nDefaultPricesPerItem, nDefaultTagsPerItem)
			blob, err := gsbm.MarshalWithOptions(&batch, 1, gsbm.Options{Compress: true})
			if err != nil {
				b.Fatalf("MarshalWithOptions: %v", err)
			}
			blobLen := len(blob)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				out, err := gsbm.MarshalWithOptions(&batch, 1, gsbm.Options{Compress: true})
				if err != nil {
					b.Fatalf("MarshalWithOptions: %v", err)
				}
				_ = out
			}
			b.StopTimer()
			b.ReportMetric(float64(blobLen), "bytes/blob")
		})
	}
}

// BenchmarkEncodeNestedBatch_Streaming_Zstd is the path issue #58
// flagged as the BeginLengthDelim hotspot: MarshalToWriter with
// Compress:true previously ran a recording size pass that appended one
// recordedRegions entry per nested length-delim region. After the
// analytic-SizeGSBM + WriteLength rewrite, generated code skips that
// recording entirely — this bench is the win's measuring stick.
func BenchmarkEncodeNestedBatch_Streaming_Zstd(b *testing.B) {
	for _, n := range benchItemCounts {
		b.Run(fmt.Sprintf("N=%d", n), func(b *testing.B) {
			batch := repeatednested.MakeNestedBatch(0, n, nDefaultLegsPerItem, nDefaultPricesPerItem, nDefaultTagsPerItem)
			pinned, err := gsbm.MarshalWithOptions(&batch, 1, gsbm.Options{Compress: true})
			if err != nil {
				b.Fatalf("MarshalWithOptions (pin): %v", err)
			}
			blobLen := len(pinned)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if err := gsbm.MarshalToWriter(io.Discard, &batch, 1, gsbm.Options{Compress: true}); err != nil {
					b.Fatalf("MarshalToWriter: %v", err)
				}
			}
			b.StopTimer()
			b.ReportMetric(float64(blobLen), "bytes/blob")
		})
	}
}
