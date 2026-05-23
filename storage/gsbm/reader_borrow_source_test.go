package gsbm

import (
	"bytes"
	"testing"
	"unsafe"
)

// TestBorrowSourceUncompressedIdentity pins the contract that for an
// uncompressed blob the slice returned by BorrowSource has the SAME
// underlying array as the one passed to NewReader — identity, not equality.
// Borrow-strings decoders alias this buffer; if the identity ever shifts,
// callers pinning the original src would lose lifetime coverage silently.
func TestBorrowSourceUncompressedIdentity(t *testing.T) {
	src := buildUncompressedBlob(t, rawBodyTag1String3, 0x1234)

	r := NewReader(src)
	got := r.BorrowSource()
	if unsafe.SliceData(got) != unsafe.SliceData(src) {
		t.Fatalf("BorrowSource before ReadHeader: underlying array differs from src")
	}
	if len(got) != len(src) {
		t.Fatalf("BorrowSource before ReadHeader: len=%d, want %d", len(got), len(src))
	}

	if _, _, _, err := r.ReadHeader(); err != nil {
		t.Fatalf("ReadHeader: %v", err)
	}
	got = r.BorrowSource()
	if unsafe.SliceData(got) != unsafe.SliceData(src) {
		t.Fatalf("BorrowSource after ReadHeader (uncompressed): underlying array differs from src")
	}
	if len(got) != len(src) {
		t.Fatalf("BorrowSource after ReadHeader (uncompressed): len=%d, want %d", len(got), len(src))
	}
}

// TestBorrowSourceCompressedIsDistinctAllocation pins the compressed-path
// half of the contract: after ReadHeader on a compressed blob, BorrowSource
// returns the reader-owned decompressed body — a separate allocation from
// the on-disk compressed bytes the caller handed to NewReader. A caller
// who pins only the original compressed slice would be unsafe; this test
// guards that the API exposes the right buffer.
func TestBorrowSourceCompressedIsDistinctAllocation(t *testing.T) {
	blob, err := MarshalWithOptions(pinnedMarshaler{}, 0x4321, Options{Compress: true})
	if err != nil {
		t.Fatalf("MarshalWithOptions{Compress:true}: %v", err)
	}

	r := NewReader(blob)
	// Before ReadHeader, BorrowSource still points at the compressed input.
	if pre := r.BorrowSource(); unsafe.SliceData(pre) != unsafe.SliceData(blob) {
		t.Fatalf("BorrowSource before ReadHeader: underlying array differs from compressed input")
	}

	flags, _, _, err := r.ReadHeader()
	if err != nil {
		t.Fatalf("ReadHeader: %v", err)
	}
	if flags&FlagCompressed == 0 {
		t.Fatalf("flags = %#x, want FlagCompressed set", flags)
	}

	got := r.BorrowSource()
	if unsafe.SliceData(got) == unsafe.SliceData(blob) {
		t.Fatalf("BorrowSource after compressed ReadHeader: still aliases compressed input (expected decompressed buffer)")
	}
	// The decompressed body must include the canonical "abc" payload bytes
	// from pinnedMarshaler so we know we are looking at the inflated content
	// rather than some unrelated slice.
	if !bytes.Contains(got, []byte("abc")) {
		t.Fatalf("BorrowSource buffer does not contain decoded payload bytes: %x", got)
	}
}

// TestBorrowSourceStreamingExposesBufferedBody asserts that the streaming
// constructor's internal buffer is what BorrowSource returns — there is no
// way to recover the io.Reader's bytes otherwise, and borrow-strings
// decoders aliasing that buffer need a lifetime handle.
func TestBorrowSourceStreamingExposesBufferedBody(t *testing.T) {
	blob := buildUncompressedBlob(t, rawBodyTag1String3, 0x4242)

	r, err := NewReaderFromN(bytes.NewReader(blob), len(blob))
	if err != nil {
		t.Fatalf("NewReaderFromN: %v", err)
	}
	got := r.BorrowSource()
	if got == nil {
		t.Fatal("BorrowSource on streaming reader: got nil")
	}
	if len(got) != len(blob) {
		t.Fatalf("BorrowSource on streaming reader: len=%d, want %d (header+body)", len(got), len(blob))
	}
	if !bytes.Equal(got, blob) {
		t.Fatalf("BorrowSource on streaming reader: bytes diverge from source blob")
	}
	// And critically: it must NOT be the same underlying array as the input
	// blob slice (the streaming reader read into a fresh allocation).
	if unsafe.SliceData(got) == unsafe.SliceData(blob) {
		t.Fatal("BorrowSource on streaming reader: must be a distinct allocation from caller's blob slice")
	}
}

// TestBorrowSourceDoesNotAllocate pins BorrowSource as zero-alloc. It is a
// trivial accessor; any future refactor that accidentally introduces a
// copy or slice growth would break the borrow-strings hot path (callers
// typically invoke it once per decoded value).
func TestBorrowSourceDoesNotAllocate(t *testing.T) {
	blob := buildUncompressedBlob(t, rawBodyTag1String3, 0)
	r := NewReader(blob)
	var sink []byte
	allocs := testing.AllocsPerRun(100, func() {
		sink = r.BorrowSource()
	})
	if allocs != 0 {
		t.Fatalf("BorrowSource allocs/op = %v, want 0", allocs)
	}
	if len(sink) == 0 {
		t.Fatal("BorrowSource returned empty slice — test sink not exercised")
	}
}

// TestBorrowSourceAfterResetReflectsNewBuffer documents the "always valid"
// guarantee across a Reader reused via Reset: the slice tracks whatever
// buffer the Reader is currently pointing at. A pooled-Reader caller that
// caches a BorrowSource result across Reset calls is the use-after-free
// hazard this test pins out.
func TestBorrowSourceAfterResetReflectsNewBuffer(t *testing.T) {
	first := buildUncompressedBlob(t, rawBodyTag1String3, 0)
	r := NewReader(first)
	if unsafe.SliceData(r.BorrowSource()) != unsafe.SliceData(first) {
		t.Fatal("BorrowSource: initial slice does not alias first input")
	}

	second := buildUncompressedBlob(t, rawBodyTag1String3, 1)
	r.Reset(second)
	if unsafe.SliceData(r.BorrowSource()) != unsafe.SliceData(second) {
		t.Fatal("BorrowSource: after Reset, slice does not alias second input")
	}
}
