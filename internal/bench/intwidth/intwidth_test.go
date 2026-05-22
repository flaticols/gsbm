package intwidth_test

import (
	"bytes"
	"testing"

	"go.flaticols.dev/gsbm/internal/bench/intwidth"
	"go.flaticols.dev/gsbm/storage/gsbm"
)

// TestVariantByteEquality is the wire-drift guard for the intwidth bench
// fixture: Plain (un-annotated `int`), Int32 (`type=int32`) and Int64
// (`type=int64`) must all encode the same in-memory payload to byte-
// identical wire output when the values fit in int32. The three codecs
// differ only in whether the bounds check is emitted; the bytes on the
// wire are the same.
func TestVariantByteEquality(t *testing.T) {
	rec := intwidth.MakeRecord(0)
	p := intwidth.PlainFrom(rec)
	i32 := intwidth.Int32From(rec)
	i64 := intwidth.Int64From(rec)

	bp, err := gsbm.Marshal(&p, 1)
	if err != nil {
		t.Fatalf("Marshal Plain: %v", err)
	}
	b32, err := gsbm.Marshal(&i32, 1)
	if err != nil {
		t.Fatalf("Marshal Int32: %v", err)
	}
	b64, err := gsbm.Marshal(&i64, 1)
	if err != nil {
		t.Fatalf("Marshal Int64: %v", err)
	}
	if !bytes.Equal(bp, b32) {
		t.Fatalf("Plain vs Int32 wire bytes diverged: len(p)=%d len(32)=%d", len(bp), len(b32))
	}
	if !bytes.Equal(bp, b64) {
		t.Fatalf("Plain vs Int64 wire bytes diverged: len(p)=%d len(64)=%d", len(bp), len(b64))
	}
}

// BenchmarkEncodeIntWidth measures the per-encode cost of the three
// variants. Plain and Int32 carry an inline int32 bounds check per
// field; Int64 omits it. Values stay inside the int32 range so all
// three succeed and the only delta is the bounds-check cost.
//
// Run as:
//
//	go test -bench=BenchmarkEncodeIntWidth -benchmem -count=3 \
//	    ./internal/bench/intwidth/
func BenchmarkEncodeIntWidth(b *testing.B) {
	rec := intwidth.MakeRecord(0)
	p := intwidth.PlainFrom(rec)
	i32 := intwidth.Int32From(rec)
	i64 := intwidth.Int64From(rec)

	b.Run("Plain", func(b *testing.B) {
		if _, err := gsbm.Marshal(&p, 1); err != nil {
			b.Fatal(err)
		}
		b.ReportAllocs()
		for b.Loop() {
			if _, err := gsbm.Marshal(&p, 1); err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("Int32", func(b *testing.B) {
		if _, err := gsbm.Marshal(&i32, 1); err != nil {
			b.Fatal(err)
		}
		b.ReportAllocs()
		for b.Loop() {
			if _, err := gsbm.Marshal(&i32, 1); err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("Int64", func(b *testing.B) {
		if _, err := gsbm.Marshal(&i64, 1); err != nil {
			b.Fatal(err)
		}
		b.ReportAllocs()
		for b.Loop() {
			if _, err := gsbm.Marshal(&i64, 1); err != nil {
				b.Fatal(err)
			}
		}
	})
}

// BenchmarkDecodeIntWidth measures the per-decode cost of the three
// variants on byte-identical wire input. Plain and Int32 carry an
// inline int32 bounds check on each ReadVarint; Int64 omits it.
func BenchmarkDecodeIntWidth(b *testing.B) {
	rec := intwidth.MakeRecord(0)
	p := intwidth.PlainFrom(rec)
	blob, err := gsbm.Marshal(&p, 1)
	if err != nil {
		b.Fatal(err)
	}

	b.Run("Plain", func(b *testing.B) {
		var dst intwidth.Plain
		if err := gsbm.DecodeInto(blob, &dst); err != nil {
			b.Fatal(err)
		}
		b.ReportAllocs()
		for b.Loop() {
			dst = intwidth.Plain{}
			if err := gsbm.DecodeInto(blob, &dst); err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("Int32", func(b *testing.B) {
		var dst intwidth.Int32
		if err := gsbm.DecodeInto(blob, &dst); err != nil {
			b.Fatal(err)
		}
		b.ReportAllocs()
		for b.Loop() {
			dst = intwidth.Int32{}
			if err := gsbm.DecodeInto(blob, &dst); err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("Int64", func(b *testing.B) {
		var dst intwidth.Int64
		if err := gsbm.DecodeInto(blob, &dst); err != nil {
			b.Fatal(err)
		}
		b.ReportAllocs()
		for b.Loop() {
			dst = intwidth.Int64{}
			if err := gsbm.DecodeInto(blob, &dst); err != nil {
				b.Fatal(err)
			}
		}
	})
}
