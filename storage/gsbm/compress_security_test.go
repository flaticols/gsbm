package gsbm

import (
	"bytes"
	"encoding/binary"
	"errors"
	"runtime"
	"testing"

	"github.com/klauspost/compress/zstd"
)

// craftZstdHugeFCS builds a minimal zstd frame (1 raw byte) whose frame
// header DECLARES a FrameContentSize of fcs — the shape of a "the frame
// lied about its size" decompression bomb. RFC 8878 frame layout: magic,
// frame-header descriptor (FCS_Field_Size=3 ⇒ 8-byte FCS, Single_Segment=0
// ⇒ a window descriptor byte follows), window descriptor, 8-byte FCS, then
// a last raw block of one byte.
func craftZstdHugeFCS(fcs uint64) []byte {
	b := []byte{0x28, 0xb5, 0x2f, 0xfd, 0xC0, 0x00} // magic, FHD=0xC0, window=0x00
	var fcsb [8]byte
	binary.LittleEndian.PutUint64(fcsb[:], fcs)
	b = append(b, fcsb[:]...)
	v := uint32(1) | (uint32(1) << 3) // last block, raw, size=1
	b = append(b, byte(v), byte(v>>8), byte(v>>16), 0x00)
	return b
}

// allocDelta runs fn and returns the bytes allocated during it.
func allocDelta(fn func()) uint64 {
	var m1, m2 runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&m1)
	fn()
	runtime.ReadMemStats(&m2)
	return m2.TotalAlloc - m1.TotalAlloc
}

// TestExtendedZstdFCSBombBounded is the regression for the
// FrameContentSize-bomb the adversarial review surfaced: an extended blob
// whose gsbm inflatedLen is honest-small but whose embedded zstd frame
// declares a multi-GiB FCS. Through ReadHeader, the decode must reject
// (ErrCorruptCompressedBody) WITHOUT allocating anywhere near the declared
// FCS — the cap-limited DecodeAll rejects the frame before growing its
// output buffer.
func TestExtendedZstdFCSBombBounded(t *testing.T) {
	frame := craftZstdHugeFCS(uint64(decoderMaxDecompressedSize))
	hdr := make([]byte, ExtendedHeaderSize)
	copy(hdr[0:4], Magic)
	hdr[4] = FmtVer2
	hdr[5] = uint8(CompressionZstd) | extendedHeaderBit
	binary.LittleEndian.PutUint32(hdr[8:12], uint32(len(frame))) // bodyLen
	binary.LittleEndian.PutUint32(hdr[12:16], 1)                 // inflatedLen=1 (honest-small)
	blob := append(hdr, frame...)

	var err error
	alloc := allocDelta(func() {
		_, _, _, err = NewReader(blob).ReadHeader()
	})
	if !errors.Is(err, ErrCorruptCompressedBody) {
		t.Fatalf("FCS bomb: want ErrCorruptCompressedBody, got %v", err)
	}
	if alloc > 64<<20 {
		t.Fatalf("FCS bomb drove %d MiB allocation from a %d-byte blob (declared FCS=%d)",
			alloc>>20, len(blob), decoderMaxDecompressedSize)
	}
}

// TestExtendedZstdHugeInflatedLenBounded covers the other bomb vector: the
// gsbm inflatedLen itself lies huge (the frame is tiny). The exact-length
// check rejects it, and the speculative reservation is clamped to the
// ceiling so the allocation stays bounded.
func TestExtendedZstdHugeInflatedLenBounded(t *testing.T) {
	// An honest tiny frame for "ab".
	frame := frameFor(t, CompressionZstd, []byte("ab"))
	hdr := make([]byte, ExtendedHeaderSize)
	copy(hdr[0:4], Magic)
	hdr[4] = FmtVer2
	hdr[5] = uint8(CompressionZstd) | extendedHeaderBit
	binary.LittleEndian.PutUint32(hdr[8:12], uint32(len(frame)))
	binary.LittleEndian.PutUint32(hdr[12:16], 2<<30) // claim ~2 GiB inflated
	blob := append(hdr, frame...)

	var err error
	alloc := allocDelta(func() {
		_, _, _, err = NewReader(blob).ReadHeader()
	})
	if !errors.Is(err, ErrCorruptCompressedBody) {
		t.Fatalf("huge inflatedLen: want ErrCorruptCompressedBody, got %v", err)
	}
	if alloc > 128<<20 {
		t.Fatalf("huge inflatedLen drove %d MiB allocation", alloc>>20)
	}
}

// TestExtendedZstdLargeBodyStreamingFallback pins that an honest body LARGER
// than the speculative ceiling (so it takes the streaming fallback rather
// than the cap-limited single-shot path) still decodes correctly.
func TestExtendedZstdLargeBodyStreamingFallback(t *testing.T) {
	if testing.Short() {
		t.Skip("allocates a >16 MiB body")
	}
	// Compressible but non-trivial payload above maxInflatePresize.
	raw := make([]byte, maxInflatePresize+(4<<20))
	for i := range raw {
		raw[i] = byte(i*131 + 7)
	}
	frame := frameFor(t, CompressionZstd, raw)
	hdr := make([]byte, ExtendedHeaderSize)
	copy(hdr[0:4], Magic)
	hdr[4] = FmtVer2
	hdr[5] = uint8(CompressionZstd) | extendedHeaderBit
	binary.LittleEndian.PutUint32(hdr[8:12], uint32(len(frame)))
	binary.LittleEndian.PutUint32(hdr[12:16], uint32(len(raw)))
	blob := append(hdr, frame...)

	out, err := decompressBody(CompressionZstd, frame, len(raw))
	if err != nil {
		t.Fatalf("large-body streaming fallback failed: %v", err)
	}
	if !bytes.Equal(out, raw) {
		t.Fatalf("large-body decode mismatch: got %d bytes", len(out))
	}
	// And end-to-end through ReadHeader.
	r := NewReader(blob)
	if _, _, _, err := r.ReadHeader(); err != nil {
		t.Fatalf("large-body ReadHeader: %v", err)
	}
}

// TestExtendedZstdCapLimitedDecoderTight confirms the cap-limited path's
// alloc-tightness assumption: a freshly cap-limited decoder fed a buffer of
// exactly inflatedLen does NOT over-grow on an honest frame.
func TestExtendedZstdCapLimitedDecoderTight(t *testing.T) {
	raw := bytes.Repeat([]byte("tight-alloc-check;"), 4096) // ~73 KiB
	frame := frameFor(t, CompressionZstd, raw)
	dec, err := zstd.NewReader(nil, zstd.WithDecodeAllCapLimit(true))
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Close()
	out, err := dec.DecodeAll(frame, make([]byte, 0, len(raw)))
	if err != nil {
		t.Fatalf("cap-limited DecodeAll: %v", err)
	}
	if !bytes.Equal(out, raw) {
		t.Fatal("cap-limited DecodeAll produced wrong bytes")
	}
}

// TestExtendedNewReaderFromTruncatedInExtField pins that a stream ending
// INSIDE the 4-byte inflatedLen field (offsets 12..15) surfaces
// ErrTruncated — never a panic or a leaked EOF. (Adversarial-probe origin.)
func TestExtendedNewReaderFromTruncatedInExtField(t *testing.T) {
	var buf bytes.Buffer
	if err := MarshalToWriter(&buf, pinnedMarshaler{}, 0xBEEF, Options{Compression: CompressionZstd}); err != nil {
		t.Fatalf("MarshalToWriter: %v", err)
	}
	full := buf.Bytes()
	if full[5]&extendedHeaderBit == 0 {
		t.Fatalf("expected extended bit set, flags=%#x", full[5])
	}
	for _, n := range []int{12, 13, 14, 15} {
		if _, err := NewReaderFrom(bytes.NewReader(full[:n])); !errors.Is(err, ErrTruncated) {
			t.Fatalf("n=%d: want ErrTruncated, got %v", n, err)
		}
	}
}

// TestExtendedBodyLenMismatch pins the headerLen=16 branch of the bodyLen
// cross-check: a 16-byte extended header whose bodyLen disagrees with the
// on-disk frame length surfaces ErrBodyLenMismatch, not a decode attempt.
func TestExtendedBodyLenMismatch(t *testing.T) {
	for _, method := range []CompressionMethod{CompressionZstd, CompressionGzip} {
		blob := buildExtendedBlob(t, method, rawBodyTag1String3, 0, uint32(len(rawBodyTag1String3)))
		realBodyLen := binary.LittleEndian.Uint32(blob[8:12])
		binary.LittleEndian.PutUint32(blob[8:12], realBodyLen-1)
		if _, _, _, err := NewReader(blob).ReadHeader(); !errors.Is(err, ErrBodyLenMismatch) {
			t.Fatalf("method %d: want ErrBodyLenMismatch, got %v", method, err)
		}
	}
}

// TestExtendedHeaderShortSlice pins the extended-truncation guard: a blob
// whose total length is between 12 and 15 bytes (extended bit set, but the
// 16-byte header is incomplete) surfaces ErrTruncated.
func TestExtendedHeaderShortSlice(t *testing.T) {
	for total := 12; total <= 15; total++ {
		blob := make([]byte, total)
		copy(blob, Magic)
		blob[4] = FmtVer2
		blob[5] = uint8(CompressionZstd) | extendedHeaderBit
		if _, _, _, err := NewReader(blob).ReadHeader(); !errors.Is(err, ErrTruncated) {
			t.Fatalf("total=%d: want ErrTruncated, got %v", total, err)
		}
	}
}
