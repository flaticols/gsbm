package borrowstrings

import (
	"fmt"
	"testing"

	"go.flaticols.dev/gsbm/storage/gsbm"
)

const (
	largeStringRecordN         = 14000
	largeStringRecordTargetMin = 1 << 20
	largeStringRecordTargetMax = 2 << 20
)

func makePlainLargeStringRecord() PlainRecord {
	note := "note-borrow-strings-large-fixture"
	r := PlainRecord{
		ID:     "record-borrow-strings-large-fixture",
		Note:   &note,
		Names:  make([]string, largeStringRecordN),
		Labels: make([]Label, largeStringRecordN),
		Tags:   make(map[string]string, largeStringRecordN),
	}
	for i := 0; i < largeStringRecordN; i++ {
		r.Names[i] = fmt.Sprintf("name-%07d-borrow-string-fixture", i)
		r.Labels[i] = Label(fmt.Sprintf("label-%07d-borrow-string-fixture", i))
		r.Tags[fmt.Sprintf("key-%07d-borrow-string-fixture", i)] = fmt.Sprintf("value-%07d-borrow-string-fixture", i)
	}
	return r
}

func makeBorrowLargeStringRecord() BorrowRecord {
	p := makePlainLargeStringRecord()
	return BorrowRecord{
		ID:     p.ID,
		Note:   p.Note,
		Names:  p.Names,
		Labels: p.Labels,
		Tags:   p.Tags,
	}
}

func encodePlainLargeBody(tb testing.TB) []byte {
	tb.Helper()
	v := makePlainLargeStringRecord()
	w := gsbm.NewWriter(nil)
	if err := v.MarshalGSBM(w); err != nil {
		tb.Fatalf("MarshalGSBM: %v", err)
	}
	if err := w.Err(); err != nil {
		tb.Fatalf("writer err: %v", err)
	}
	body := w.Bytes()
	if len(body) < largeStringRecordTargetMin || len(body) > largeStringRecordTargetMax {
		tb.Fatalf("large plain string fixture body size %d out of target [%d, %d]", len(body), largeStringRecordTargetMin, largeStringRecordTargetMax)
	}
	return body
}

func encodeBorrowLargeBody(tb testing.TB) []byte {
	tb.Helper()
	v := makeBorrowLargeStringRecord()
	w := gsbm.NewWriter(nil)
	if err := v.MarshalGSBM(w); err != nil {
		tb.Fatalf("MarshalGSBM: %v", err)
	}
	if err := w.Err(); err != nil {
		tb.Fatalf("writer err: %v", err)
	}
	body := w.Bytes()
	if len(body) < largeStringRecordTargetMin || len(body) > largeStringRecordTargetMax {
		tb.Fatalf("large borrow string fixture body size %d out of target [%d, %d]", len(body), largeStringRecordTargetMin, largeStringRecordTargetMax)
	}
	return body
}

func TestLargeStringRecordBorrowAllocDrop(t *testing.T) {
	if testing.Short() {
		t.Skip("alloc budget runs full")
	}
	plainBody := encodePlainLargeBody(t)
	borrowBody := encodeBorrowLargeBody(t)
	var plain PlainRecord
	var borrow BorrowRecord
	if err := gsbm.DecodeBodyInto(plainBody, &plain); err != nil {
		t.Fatal(err)
	}
	if err := gsbm.DecodeBodyInto(borrowBody, &borrow); err != nil {
		t.Fatal(err)
	}
	plainAllocs := testing.AllocsPerRun(5, func() {
		if err := gsbm.DecodeBodyInto(plainBody, &plain); err != nil {
			t.Fatal(err)
		}
	})
	borrowAllocs := testing.AllocsPerRun(5, func() {
		if err := gsbm.DecodeBodyInto(borrowBody, &borrow); err != nil {
			t.Fatal(err)
		}
	})
	if plainAllocs < 10000 {
		t.Fatalf("plain fixture allocs/op %.2f unexpectedly low; benchmark no longer proves string-copy removal", plainAllocs)
	}
	if borrowAllocs > 500 {
		t.Fatalf("borrow fixture allocs/op %.2f, want <= 500", borrowAllocs)
	}
	if borrowAllocs >= plainAllocs/10 {
		t.Fatalf("borrow allocs/op %.2f did not drop below 10%% of plain %.2f", borrowAllocs, plainAllocs)
	}
	t.Logf("plain %.2f allocs/op; borrow %.2f allocs/op", plainAllocs, borrowAllocs)
}

func BenchmarkLargeStringRecordDecodeHeapPlainWarm(b *testing.B) {
	body := encodePlainLargeBody(b)
	b.SetBytes(int64(len(body)))
	var dst PlainRecord
	if err := gsbm.DecodeBodyInto(body, &dst); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for b.Loop() {
		if err := gsbm.DecodeBodyInto(body, &dst); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkLargeStringRecordDecodeHeapBorrowWarm(b *testing.B) {
	body := encodeBorrowLargeBody(b)
	b.SetBytes(int64(len(body)))
	var dst BorrowRecord
	if err := gsbm.DecodeBodyInto(body, &dst); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for b.Loop() {
		if err := gsbm.DecodeBodyInto(body, &dst); err != nil {
			b.Fatal(err)
		}
	}
}
