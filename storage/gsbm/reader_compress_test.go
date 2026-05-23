package gsbm

import (
	"bytes"
	"encoding/binary"
	"errors"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/klauspost/compress/zstd"
)

// rawBodyTag1String3 is the wire-form payload used by several compressed-blob
// tests: a single tag=1, WireLengthDelim field carrying the string "abc".
// Pre-computed so each test sees the same exact bytes.
var rawBodyTag1String3 = []byte{0x0A, 0x03, 0x61, 0x62, 0x63}

// buildCompressedBlob produces a fmtVer-2 blob whose body is the zstd frame
// of raw, with flags = FlagCompressed and bodyLen = len(compressed). Built
// in test code so each compressed-blob test sees a fully hand-rolled blob
// (no dependence on a not-yet-shipped MarshalWithOptions writer path).
func buildCompressedBlob(t *testing.T, raw []byte, schemaHint uint16) []byte {
	t.Helper()
	enc, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedFastest))
	if err != nil {
		t.Fatalf("zstd.NewWriter: %v", err)
	}
	defer func() { _ = enc.Close() }()
	compressed := enc.EncodeAll(raw, nil)
	if len(compressed) == 0 {
		t.Fatalf("zstd produced empty output for %d-byte input", len(raw))
	}
	blob := make([]byte, 0, HeaderSize+len(compressed))
	blob = append(blob, Magic[0], Magic[1], Magic[2], Magic[3], FmtVer2, FlagCompressed)
	blob = binary.LittleEndian.AppendUint16(blob, schemaHint)
	blob = binary.LittleEndian.AppendUint32(blob, uint32(len(compressed)))
	blob = append(blob, compressed...)
	return blob
}

// TestReadHeaderAcceptsCompressedBlob round-trips a hand-rolled flags=0x01
// blob through ReadHeader: the decoder must accept the flag, decompress the
// body in place, and let subsequent primitive reads see the inflated bytes
// as if the blob had been written uncompressed.
func TestReadHeaderAcceptsCompressedBlob(t *testing.T) {
	blob := buildCompressedBlob(t, rawBodyTag1String3, 0x1234)

	r := NewReader(blob)
	flags, schemaHint, bodyLen, err := r.ReadHeader()
	if err != nil {
		t.Fatalf("ReadHeader: %v", err)
	}
	if flags != FlagCompressed {
		t.Fatalf("flags = %#x, want %#x", flags, FlagCompressed)
	}
	if schemaHint != 0x1234 {
		t.Fatalf("schemaHint = %#x, want 0x1234", schemaHint)
	}
	// bodyLen reflects on-disk (compressed) length per spec §2.1.
	wantBodyLen := uint32(len(blob) - HeaderSize)
	if bodyLen != wantBodyLen {
		t.Fatalf("bodyLen = %d, want %d (compressed body length)", bodyLen, wantBodyLen)
	}

	// After ReadHeader the Reader view is the decompressed body; reads
	// proceed exactly as for an uncompressed blob.
	tag, wt, err := r.ReadTag()
	if err != nil {
		t.Fatalf("ReadTag: %v", err)
	}
	if tag != 1 || wt != WireLengthDelim {
		t.Fatalf("tag=%d wt=%d, want 1/LengthDelim", tag, wt)
	}
	s, err := r.ReadString()
	if err != nil {
		t.Fatalf("ReadString: %v", err)
	}
	if s != "abc" {
		t.Fatalf("string = %q, want %q", s, "abc")
	}
	if r.HasMore() {
		t.Fatal("trailing bytes after decoded body")
	}
}

// TestReadHeaderRejectsReservedFlagsBits1Through7 asserts that every bit
// outside FlagCompressed remains reserved. Forward-compat contract: a
// future encoder that sets bit 1 (e.g., a second compression codec) must
// be rejected by this PR's reader rather than silently mis-decoded.
func TestReadHeaderRejectsReservedFlagsBits1Through7(t *testing.T) {
	for bit := 1; bit <= 7; bit++ {
		flags := byte(1 << bit)
		// Empty body so the cross-check passes and the flags check is the
		// one that fires.
		bad := []byte{'G', 'S', 'B', 'M', FmtVer2, flags, 0, 0, 0, 0, 0, 0}
		if _, _, _, err := NewReader(bad).ReadHeader(); !errors.Is(err, ErrReservedFlags) {
			t.Fatalf("flags=%#x: want ErrReservedFlags, got %v", flags, err)
		}
	}

	// Combinations that include bit 0 plus a reserved bit must also reject:
	// the reserved-bit semantics dominate the compressed-marker semantics.
	for _, flags := range []byte{0x03, 0x81, 0xFF} {
		bad := []byte{'G', 'S', 'B', 'M', FmtVer2, flags, 0, 0, 0, 0, 0, 0}
		if _, _, _, err := NewReader(bad).ReadHeader(); !errors.Is(err, ErrReservedFlags) {
			t.Fatalf("flags=%#x: want ErrReservedFlags, got %v", flags, err)
		}
	}
}

// TestReadHeaderRejectsTruncatedCompressedBody asserts that a flags=0x01
// blob with an incomplete zstd frame surfaces as ErrCorruptCompressedBody
// rather than panicking inside the decoder. The cross-check still passes
// because bodyLen reflects the on-disk truncated length; the failure must
// come from the decompression step.
func TestReadHeaderRejectsTruncatedCompressedBody(t *testing.T) {
	full := buildCompressedBlob(t, rawBodyTag1String3, 0x0001)
	// Drop the last byte of the compressed body, then patch bodyLen to
	// match so the cross-check passes and we exercise the zstd decoder
	// failure path (not the bodyLen mismatch path).
	truncated := full[:len(full)-1]
	newBodyLen := uint32(len(truncated) - HeaderSize)
	binary.LittleEndian.PutUint32(truncated[8:12], newBodyLen)

	_, _, _, err := NewReader(truncated).ReadHeader()
	if !errors.Is(err, ErrCorruptCompressedBody) {
		t.Fatalf("truncated zstd body: want ErrCorruptCompressedBody, got %v", err)
	}
}

// TestReadHeaderRejectsMalformedZstdMagic asserts that flags=0x01 with a
// body that fails the zstd frame magic check surfaces as
// ErrCorruptCompressedBody.
func TestReadHeaderRejectsMalformedZstdMagic(t *testing.T) {
	// A 5-byte body of random non-zstd bytes (zstd frame magic is
	// 28 B5 2F FD — the values below are intentionally none of that).
	body := []byte{0xDE, 0xAD, 0xBE, 0xEF, 0x00}
	blob := []byte{'G', 'S', 'B', 'M', FmtVer2, FlagCompressed, 0, 0}
	blob = binary.LittleEndian.AppendUint32(blob, uint32(len(body)))
	blob = append(blob, body...)

	_, _, _, err := NewReader(blob).ReadHeader()
	if !errors.Is(err, ErrCorruptCompressedBody) {
		t.Fatalf("malformed zstd body: want ErrCorruptCompressedBody, got %v", err)
	}
}

// TestReadHeaderRejectsDecompressionBomb pins the decompression-bomb
// safety contract: a compressed body whose declared decompressed size
// exceeds the decoder's WithDecoderMaxMemory cap must surface as
// ErrCorruptCompressedBody, not as an unbounded allocation. Without the
// cap, klauspost's default 64 GiB per-DecodeAll budget lets a hostile
// blob OOM the process even though the on-disk bodyLen is tiny — a
// violation of spec.md §8's panic-free hostile-input rule.
//
// The frame is hand-rolled to keep the test cheap: the decoder rejects
// on Frame_Content_Size > max before it allocates the output buffer or
// reads any data block, so we only need a valid frame header that
// declares a bomb-sized FCS. RFC 8478 §3.1.1 layout:
//
//	magic [4] = 28 B5 2F FD
//	Frame_Header_Descriptor [1] = 0xE0 (FCS_flag=3 → 8-byte FCS,
//	    Single_Segment=1, no Content_Checksum, no Dictionary_ID)
//	Frame_Content_Size [8] = uint64 LE, > decoderMaxDecompressedSize
//	Block_Header [3] = 01 00 00 (Last_Block=1, Raw, size=0)
func TestReadHeaderRejectsDecompressionBomb(t *testing.T) {
	const bombFCS = uint64(math.MaxUint32) + 1
	frame := []byte{0x28, 0xB5, 0x2F, 0xFD, 0xE0}
	frame = binary.LittleEndian.AppendUint64(frame, bombFCS)
	frame = append(frame, 0x01, 0x00, 0x00) // last raw block, 0 bytes

	blob := make([]byte, 0, HeaderSize+len(frame))
	blob = append(blob, Magic[0], Magic[1], Magic[2], Magic[3], FmtVer2, FlagCompressed)
	blob = binary.LittleEndian.AppendUint16(blob, 0)
	blob = binary.LittleEndian.AppendUint32(blob, uint32(len(frame)))
	blob = append(blob, frame...)

	_, _, _, gotErr := NewReader(blob).ReadHeader()
	if !errors.Is(gotErr, ErrCorruptCompressedBody) {
		t.Fatalf("decompression bomb: want ErrCorruptCompressedBody, got %v", gotErr)
	}
}

// oldReaderRejectFlags mimics the pre-PR reader's flags-validation rule
// (flags != 0 → reject). Kept in test code so the production reader can
// widen its rule to (flags & 0xFE) != 0 without losing the regression
// guarantee that a *previously-shipped* binary would have refused the
// new compressed blob cleanly (forward-compat: clean failure, not silent
// corruption — see plan §"Compatibility matrix" and §"Forward compat").
func oldReaderRejectFlags(blob []byte) error {
	if len(blob) < HeaderSize {
		return ErrTruncated
	}
	if string(blob[0:4]) != Magic {
		return ErrBadMagic
	}
	if blob[4] != FmtVer2 {
		return ErrUnsupportedVer
	}
	if blob[5] != 0 {
		return ErrReservedFlags
	}
	return nil
}

// TestOldReaderRejectsNewCompressedBlob pins the forward-compat contract:
// a binary still running the pre-PR reader (simulated by oldReaderRejectFlags)
// must reject a flags=0x01 compressed blob with ErrReservedFlags rather than
// silently decoding the zstd-framed body as raw wire bytes. The spec reserved
// bit 0 precisely so this transition could ship without corrupting old readers.
func TestOldReaderRejectsNewCompressedBlob(t *testing.T) {
	blob, err := MarshalWithOptions(pinnedMarshaler{}, 0x1234, Options{Compress: true})
	if err != nil {
		t.Fatalf("MarshalWithOptions{Compress:true}: %v", err)
	}
	if err := oldReaderRejectFlags(blob); !errors.Is(err, ErrReservedFlags) {
		t.Fatalf("old reader on compressed blob: want ErrReservedFlags, got %v", err)
	}
	// Sanity: the same simulated old reader still accepts a flags=0 blob.
	uncompressed, err := Marshal(pinnedMarshaler{}, 0x1234)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if err := oldReaderRejectFlags(uncompressed); err != nil {
		t.Fatalf("old reader on uncompressed blob: %v", err)
	}
}

// TestReadHeaderLegacyUncompressedPinned is the backward-compat regression:
// a pinned fmtVer=2, flags=0 blob in testdata/legacy_uncompressed.bin must
// decode byte-for-byte the same way through ReadHeader + primitive reads
// after this PR as it did before. If this test fails, the wire format has
// shifted under existing readers — investigate before changing the test.
func TestReadHeaderLegacyUncompressedPinned(t *testing.T) {
	path := filepath.Join("testdata", "legacy_uncompressed.bin")
	blob, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	// Pin the on-disk bytes so an unintended testdata edit fails loudly.
	want := []byte{
		'G', 'S', 'B', 'M', FmtVer2, 0x00, // magic, fmtVer, flags
		0x34, 0x12, // schemaHint 0x1234 LE
		0x07, 0x00, 0x00, 0x00, // bodyLen = 7 LE
		0x0A, 0x03, 0x61, 0x62, 0x63, // tag=1 lendelim, "abc"
		0x10, 0x2A, // tag=2 varint, 42
	}
	if !bytes.Equal(blob, want) {
		t.Fatalf("legacy blob bytes changed:\n got %x\nwant %x", blob, want)
	}

	r := NewReader(blob)
	flags, schemaHint, bodyLen, err := r.ReadHeader()
	if err != nil {
		t.Fatalf("ReadHeader: %v", err)
	}
	if flags != 0 || schemaHint != 0x1234 || bodyLen != 7 {
		t.Fatalf("header mismatch: flags=%#x schemaHint=%#x bodyLen=%d", flags, schemaHint, bodyLen)
	}

	tag, wt, err := r.ReadTag()
	if err != nil {
		t.Fatalf("ReadTag #1: %v", err)
	}
	if tag != 1 || wt != WireLengthDelim {
		t.Fatalf("field #1 key: tag=%d wt=%d, want 1/LengthDelim", tag, wt)
	}
	s, err := r.ReadString()
	if err != nil {
		t.Fatalf("ReadString: %v", err)
	}
	if s != "abc" {
		t.Fatalf("field #1 value = %q, want %q", s, "abc")
	}

	tag, wt, err = r.ReadTag()
	if err != nil {
		t.Fatalf("ReadTag #2: %v", err)
	}
	if tag != 2 || wt != WireVarint {
		t.Fatalf("field #2 key: tag=%d wt=%d, want 2/Varint", tag, wt)
	}
	v, err := r.ReadUvarint()
	if err != nil {
		t.Fatalf("ReadUvarint: %v", err)
	}
	if v != 42 {
		t.Fatalf("field #2 value = %d, want 42", v)
	}

	if r.HasMore() {
		t.Fatal("trailing bytes after decoded legacy body")
	}
}
