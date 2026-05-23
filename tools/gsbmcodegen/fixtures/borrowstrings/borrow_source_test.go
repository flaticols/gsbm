package borrowstrings

import (
	"runtime"
	"testing"
	"unsafe"

	"go.flaticols.dev/gsbm/storage/gsbm"
)

// fixtureBorrowRecord returns the canonical borrow-string fixture value
// shared by every lifetime test in this file. Centralising it keeps the
// string contents identical across uncompressed / compressed / arena cases,
// so the post-GC equality assertions can pin exact values.
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

// assertBorrowRecordValues checks every string-bearing field of got against
// the canonical fixture. Used after GC pressure to assert that borrowed
// strings have not been corrupted.
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

// TestBorrowSourceLifetimeUncompressed is the end-to-end lifetime contract
// for the uncompressed path: a borrow-string decoder aliases the original
// src; after dropping the decoder and forcing GC, pinning r.BorrowSource()
// via runtime.KeepAlive keeps the borrowed strings valid.
//
// The buffer returned by BorrowSource must be the same underlying array as
// the input blob (identity, not equality) so callers who already retain
// `blob` for other reasons can rely on existing references rather than
// taking a new one.
func TestBorrowSourceLifetimeUncompressed(t *testing.T) {
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
	// Identity claim from the BorrowSource contract: uncompressed → same
	// underlying array as src.
	if unsafe.SliceData(body) != unsafe.SliceData(blob) {
		t.Fatalf("uncompressed BorrowSource: underlying array differs from blob")
	}
	// Sanity: at least one borrowed string actually aliases the body.
	if !aliasesBuffer(got.ID, body) {
		t.Fatalf("uncompressed: got.ID does not alias BorrowSource buffer")
	}

	// Drop the Reader so only `body` and `got` retain references to the
	// decode source. GC twice — once to mark, once to actually sweep any
	// finalizer-bound heap.
	r = nil
	_ = r
	runtime.GC()
	runtime.GC()

	assertBorrowRecordValues(t, &got)
	runtime.KeepAlive(body)
}

// TestBorrowSourceLifetimeCompressed is the same lifetime story for a
// compressed blob: the buffer that backs borrowed strings is the
// reader-allocated decompressed body, not the on-disk compressed bytes —
// callers MUST pin r.BorrowSource(), not the original blob.
func TestBorrowSourceLifetimeCompressed(t *testing.T) {
	blob, err := gsbm.MarshalWithOptions(fixtureBorrowRecord(), 0x1234, gsbm.Options{Compress: true})
	if err != nil {
		t.Fatalf("MarshalWithOptions{Compress:true}: %v", err)
	}

	r := gsbm.NewReader(blob)
	flags, _, _, err := r.ReadHeader()
	if err != nil {
		t.Fatalf("ReadHeader: %v", err)
	}
	if flags&gsbm.FlagCompressed == 0 {
		t.Fatalf("flags = %#x, want FlagCompressed set", flags)
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
	// Distinct-allocation claim for compressed: BorrowSource is NOT the
	// compressed input.
	if unsafe.SliceData(body) == unsafe.SliceData(blob) {
		t.Fatalf("compressed BorrowSource: must not alias compressed input")
	}
	// Borrowed strings alias the decompressed buffer.
	if !aliasesBuffer(got.ID, body) {
		t.Fatalf("compressed: got.ID does not alias BorrowSource buffer")
	}
	if aliasesBuffer(got.ID, blob) {
		t.Fatalf("compressed: got.ID unexpectedly aliases compressed input")
	}

	r = nil
	blob = nil
	_ = r
	_ = blob
	runtime.GC()
	runtime.GC()

	assertBorrowRecordValues(t, &got)
	runtime.KeepAlive(body)
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
