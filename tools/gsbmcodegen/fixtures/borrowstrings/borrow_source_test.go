package borrowstrings

import (
	"testing"
	"unsafe"

	"go.flaticols.dev/gsbm/storage/gsbm"
)

func fixtureBorrowRecord() *BorrowRecord {
	note := "bravo-note"
	return &BorrowRecord{
		ID:     "alpha-id",
		Note:   &note,
		Names:  []string{"charlie-name"},
		Labels: []Label{Label("delta-label")},
		Tags:   map[string]string{"echo-key": "foxtrot-value"},
	}
}

func assertBorrowRecordValues(t *testing.T, got *BorrowRecord) {
	t.Helper()
	if got.ID != "alpha-id" {
		t.Fatalf("ID = %q, want %q", got.ID, "alpha-id")
	}
	if got.Note == nil || *got.Note != "bravo-note" {
		t.Fatalf("Note = %v, want %q", got.Note, "bravo-note")
	}
	if len(got.Names) != 1 || got.Names[0] != "charlie-name" {
		t.Fatalf("Names = %q, want [charlie-name]", got.Names)
	}
	if len(got.Labels) != 1 || string(got.Labels[0]) != "delta-label" {
		t.Fatalf("Labels = %q, want [delta-label]", got.Labels)
	}
	if v, ok := got.Tags["echo-key"]; !ok || v != "foxtrot-value" {
		t.Fatalf("Tags[echo-key] = %q ok=%v, want foxtrot-value/true", v, ok)
	}
}

// TestBorrowSourceMutationCorruptsBorrowsUncompressed proves the borrow
// contract by violating it: mutating bytes in the slice returned by
// BorrowSource must corrupt the borrowed string fields in place. This is
// the inverse of TestBorrowSourceArenaModeIsIndependent (where allocator-
// owned strings are unaffected by the same mutation) and pins the
// "borrowed strings alias this buffer" claim with a falsifiable test —
// runtime.GC()-based lifetime tests cannot do that portably.
func TestBorrowSourceMutationCorruptsBorrowsUncompressed(t *testing.T) {
	blob, err := gsbm.MarshalWithOptions(fixtureBorrowRecord(), 0x1234, gsbm.Options{})
	if err != nil {
		t.Fatalf("MarshalWithOptions: %v", err)
	}

	r := gsbm.NewReader(blob)
	if _, _, _, err := r.ReadHeader(); err != nil {
		t.Fatalf("ReadHeader: %v", err)
	}
	var got BorrowRecord
	got.Reset()
	if err := got.UnmarshalGSBM(r); err != nil {
		t.Fatalf("UnmarshalGSBM: %v", err)
	}
	if err := r.Err(); err != nil {
		t.Fatalf("reader err: %v", err)
	}

	body := r.BorrowSource()
	if unsafe.SliceData(body) != unsafe.SliceData(blob) {
		t.Fatalf("uncompressed BorrowSource: underlying array differs from blob")
	}
	if !aliasesBuffer(got.ID, body) {
		t.Fatalf("uncompressed: got.ID does not alias BorrowSource buffer")
	}

	for i := range body {
		body[i] = 0xFF
	}
	if got.ID == "alpha-id" {
		t.Fatalf("borrowed ID survived BorrowSource mutation — alias contract broken")
	}
}

// TestBorrowSourceMutationCorruptsBorrowsCompressed is the compressed
// variant: borrowed strings alias the reader-allocated decompressed body,
// not the on-disk compressed bytes — so mutating the original `blob`
// leaves borrowed strings untouched, while mutating BorrowSource() corrupts
// them. Both directions are asserted.
func TestBorrowSourceMutationCorruptsBorrowsCompressed(t *testing.T) {
	blob, err := gsbm.MarshalWithOptions(fixtureBorrowRecord(), 0x1234, gsbm.Options{Compress: true})
	if err != nil {
		t.Fatalf("MarshalWithOptions{Compress:true}: %v", err)
	}

	r := gsbm.NewReader(blob)
	flags, _, _, err := r.ReadHeader()
	if err != nil {
		t.Fatalf("ReadHeader: %v", err)
	}
	if gsbm.CompressionMethod(flags) == gsbm.CompressionNone {
		t.Fatalf("flags = %#x, want a compressing codec set", flags)
	}
	var got BorrowRecord
	got.Reset()
	if err := got.UnmarshalGSBM(r); err != nil {
		t.Fatalf("UnmarshalGSBM: %v", err)
	}
	if err := r.Err(); err != nil {
		t.Fatalf("reader err: %v", err)
	}

	body := r.BorrowSource()
	if unsafe.SliceData(body) == unsafe.SliceData(blob) {
		t.Fatalf("compressed BorrowSource: must not alias compressed input")
	}
	if !aliasesBuffer(got.ID, body) {
		t.Fatalf("compressed: got.ID does not alias BorrowSource buffer")
	}
	if aliasesBuffer(got.ID, blob) {
		t.Fatalf("compressed: got.ID unexpectedly aliases compressed input")
	}

	// Trashing the compressed input must NOT touch borrowed strings — they
	// live in the decompressed buffer.
	for i := range blob {
		blob[i] = 0x00
	}
	assertBorrowRecordValues(t, &got)

	// Trashing the decompressed buffer MUST corrupt them.
	for i := range body {
		body[i] = 0xFF
	}
	if got.ID == "alpha-id" {
		t.Fatalf("borrowed ID survived BorrowSource mutation on decompressed buffer — alias contract broken")
	}
}

// TestBorrowSourceArenaModeIsIndependent pins the documented exception:
// in arena mode (Allocator != nil), borrowed strings are owned by the
// allocator rather than aliasing r.buf — BorrowSource is still callable
// (consistent API) but pinning it is not required. We prove that by
// mutating the BorrowSource buffer after decode and showing the decoded
// strings are unaffected.
func TestBorrowSourceArenaModeIsIndependent(t *testing.T) {
	blob := encodeBorrow(t, fixtureBorrowRecord())

	r := gsbm.NewReader(blob)
	r.SetAllocator(copyingAllocator{})
	var got BorrowRecord
	got.Reset()
	if err := got.UnmarshalGSBM(r); err != nil {
		t.Fatalf("UnmarshalGSBM: %v", err)
	}
	if err := r.Err(); err != nil {
		t.Fatalf("reader err: %v", err)
	}

	body := r.BorrowSource()
	if body == nil {
		t.Fatal("BorrowSource returned nil in arena mode (expected the buffer for consistency)")
	}
	// None of got's strings may alias body — allocator owns them.
	if aliasesBuffer(got.ID, body) {
		t.Fatalf("arena: got.ID unexpectedly aliases BorrowSource buffer")
	}

	// Trash every byte the borrow path could have aliased. With the arena
	// allocator the decoded strings are independent copies, so they must
	// survive this in full.
	for i := range body {
		body[i] = 0xFF
	}
	assertBorrowRecordValues(t, &got)
}
