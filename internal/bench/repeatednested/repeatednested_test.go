package repeatednested_test

import (
	"fmt"
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
