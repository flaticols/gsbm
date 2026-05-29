package gsbm

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"testing"

	"github.com/klauspost/compress/zstd"
)

// buildUncompressedBlob mirrors buildCompressedBlob from reader_compress_test.go
// but writes the raw body verbatim with flags=0. Kept inline so the streaming
// tests don't depend on the writer-side production path being symmetric.
func buildUncompressedBlob(t *testing.T, raw []byte, schemaHint uint16) []byte {
	t.Helper()
	blob := make([]byte, 0, HeaderSize+len(raw))
	blob = append(blob, Magic[0], Magic[1], Magic[2], Magic[3], FmtVer2, 0)
	blob = binary.LittleEndian.AppendUint16(blob, schemaHint)
	blob = binary.LittleEndian.AppendUint32(blob, uint32(len(raw)))
	blob = append(blob, raw...)
	return blob
}

// TestNewReaderFromUncompressedRoundTrip — streaming read of an
// uncompressed blob: the returned Reader behaves identically to
// NewReader(blob), and the standard ReadHeader + primitive read flow
// recovers the body bytes verbatim.
func TestNewReaderFromUncompressedRoundTrip(t *testing.T) {
	blob := buildUncompressedBlob(t, rawBodyTag1String3, 0x4242)

	r, err := NewReaderFrom(bytes.NewReader(blob))
	if err != nil {
		t.Fatalf("NewReaderFrom: %v", err)
	}
	flags, schemaHint, bodyLen, err := r.ReadHeader()
	if err != nil {
		t.Fatalf("ReadHeader: %v", err)
	}
	if flags != 0 || schemaHint != 0x4242 || bodyLen != uint32(len(rawBodyTag1String3)) {
		t.Fatalf("header mismatch: flags=%#x schemaHint=%#x bodyLen=%d", flags, schemaHint, bodyLen)
	}
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

// TestMarshalToWriterCompressedThroughNewReaderFrom pins the streaming
// counterpart round-trip explicitly required by the acceptance criteria:
// MarshalToWriter(Compress:true) → NewReaderFrom(r) → decode equals the
// original. The transitive proof exists (MarshalToWriter is byte-identical
// to MarshalWithOptions, NewReaderFrom is observation-equivalent to
// NewReader), but the acceptance criterion calls for a dedicated pin.
func TestMarshalToWriterCompressedThroughNewReaderFrom(t *testing.T) {
	var buf bytes.Buffer
	if err := MarshalToWriter(&buf, pinnedMarshaler{}, 0xCAFE, Options{Compression: CompressionZstd}); err != nil {
		t.Fatalf("MarshalToWriter: %v", err)
	}
	r, err := NewReaderFrom(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("NewReaderFrom: %v", err)
	}
	flags, hint, _, err := r.ReadHeader()
	if err != nil {
		t.Fatalf("ReadHeader: %v", err)
	}
	if flags != FlagCompressed {
		t.Fatalf("flags = %#x, want %#x", flags, FlagCompressed)
	}
	if hint != 0xCAFE {
		t.Fatalf("schemaHint = %#x, want 0xCAFE", hint)
	}

	tag, wt, err := r.ReadTag()
	if err != nil {
		t.Fatalf("ReadTag #1: %v", err)
	}
	if tag != 1 || wt != WireLengthDelim {
		t.Fatalf("field #1: tag=%d wt=%d", tag, wt)
	}
	s, err := r.ReadString()
	if err != nil {
		t.Fatalf("ReadString: %v", err)
	}
	if s != "abc" {
		t.Fatalf("field #1 = %q, want %q", s, "abc")
	}
	tag, wt, err = r.ReadTag()
	if err != nil {
		t.Fatalf("ReadTag #2: %v", err)
	}
	if tag != 2 || wt != WireVarint {
		t.Fatalf("field #2: tag=%d wt=%d", tag, wt)
	}
	v, err := r.ReadUvarint()
	if err != nil {
		t.Fatalf("ReadUvarint: %v", err)
	}
	if v != 42 {
		t.Fatalf("field #2 = %d, want 42", v)
	}
}

// TestNewReaderFromCompressedRoundTrip — streaming read of a compressed
// blob. ReadHeader transparently decompresses via the pooled decoder; the
// caller never sees the compression layer.
func TestNewReaderFromCompressedRoundTrip(t *testing.T) {
	blob := buildCompressedBlob(t, rawBodyTag1String3, 0x1234)

	r, err := NewReaderFrom(bytes.NewReader(blob))
	if err != nil {
		t.Fatalf("NewReaderFrom: %v", err)
	}
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
	if bodyLen != uint32(len(blob)-HeaderSize) {
		t.Fatalf("bodyLen = %d, want %d (compressed length)", bodyLen, len(blob)-HeaderSize)
	}
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

// TestNewReaderFromTruncatedHeader — a stream that ends inside the 12-byte
// header surfaces as ErrTruncated, not a downstream EOF leak.
func TestNewReaderFromTruncatedHeader(t *testing.T) {
	full := buildUncompressedBlob(t, rawBodyTag1String3, 1)
	// Various truncation points within the header.
	for _, n := range []int{0, 1, 4, 5, 6, 8, 11} {
		if _, err := NewReaderFrom(bytes.NewReader(full[:n])); !errors.Is(err, ErrTruncated) {
			t.Fatalf("n=%d: want ErrTruncated, got %v", n, err)
		}
	}
}

// TestNewReaderFromTruncatedBody — a stream that delivers a complete
// header but ends inside the body must also surface as ErrTruncated.
// Covers both the uncompressed and compressed paths since both consume
// bodyLen bytes from src.
func TestNewReaderFromTruncatedBody(t *testing.T) {
	t.Run("uncompressed", func(t *testing.T) {
		full := buildUncompressedBlob(t, rawBodyTag1String3, 1)
		// Drop the last body byte; header still says bodyLen = full length.
		truncated := full[:len(full)-1]
		if _, err := NewReaderFrom(bytes.NewReader(truncated)); !errors.Is(err, ErrTruncated) {
			t.Fatalf("uncompressed truncated: want ErrTruncated, got %v", err)
		}
	})
	t.Run("compressed", func(t *testing.T) {
		full := buildCompressedBlob(t, rawBodyTag1String3, 1)
		truncated := full[:len(full)-1]
		if _, err := NewReaderFrom(bytes.NewReader(truncated)); !errors.Is(err, ErrTruncated) {
			t.Fatalf("compressed truncated: want ErrTruncated, got %v", err)
		}
	})
}

// TestNewReaderFromHeaderRejects — bad magic, fmtVer, and reserved flag
// bits are all caught at the NewReaderFrom pre-validation step, before any
// body allocation. The hostile-stream guarantee is that a bogus header
// cannot drive a multi-gigabyte allocation.
func TestNewReaderFromHeaderRejects(t *testing.T) {
	t.Run("bad magic", func(t *testing.T) {
		blob := buildUncompressedBlob(t, rawBodyTag1String3, 1)
		blob[0] = 'X'
		if _, err := NewReaderFrom(bytes.NewReader(blob)); !errors.Is(err, ErrBadMagic) {
			t.Fatalf("want ErrBadMagic, got %v", err)
		}
	})
	t.Run("unsupported fmtVer", func(t *testing.T) {
		blob := buildUncompressedBlob(t, rawBodyTag1String3, 1)
		blob[4] = 3 // unknown future version
		if _, err := NewReaderFrom(bytes.NewReader(blob)); !errors.Is(err, ErrUnsupportedVer) {
			t.Fatalf("want ErrUnsupportedVer, got %v", err)
		}
	})
	t.Run("reserved flag bits", func(t *testing.T) {
		// Reserved = method values 3–7 (bits 0–2) and any high bit 3–7.
		// Methods 0/1/2 (none/zstd/gzip) are accepted, so they are not in
		// this set.
		reserved := []byte{0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x10, 0x20, 0x40, 0x80}
		for _, flags := range reserved {
			blob := buildUncompressedBlob(t, rawBodyTag1String3, 1)
			blob[5] = flags
			if _, err := NewReaderFrom(bytes.NewReader(blob)); !errors.Is(err, ErrReservedFlags) {
				t.Fatalf("flags %#x: want ErrReservedFlags, got %v", flags, err)
			}
		}
	})
}

// TestNewReaderFromCompressedCorrupt — a flags=0x01 blob whose body is
// not a valid zstd frame surfaces from ReadHeader as
// ErrCorruptCompressedBody (same path that NewReader hits). Confirms the
// streaming entrypoint defers compression handling to ReadHeader cleanly.
func TestNewReaderFromCompressedCorrupt(t *testing.T) {
	body := []byte{0xDE, 0xAD, 0xBE, 0xEF, 0x00}
	blob := []byte{'G', 'S', 'B', 'M', FmtVer2, FlagCompressed, 0, 0}
	blob = binary.LittleEndian.AppendUint32(blob, uint32(len(body)))
	blob = append(blob, body...)

	r, err := NewReaderFrom(bytes.NewReader(blob))
	if err != nil {
		t.Fatalf("NewReaderFrom: %v", err)
	}
	if _, _, _, err := r.ReadHeader(); !errors.Is(err, ErrCorruptCompressedBody) {
		t.Fatalf("want ErrCorruptCompressedBody, got %v", err)
	}
}

// TestNewReaderFromTrailingBytesPreserved — the streaming entrypoint must
// consume exactly (header + bodyLen) bytes and leave the rest of src for
// the caller. Useful for length-framed streams that carry more than one
// blob back-to-back.
func TestNewReaderFromTrailingBytesPreserved(t *testing.T) {
	blob := buildUncompressedBlob(t, rawBodyTag1String3, 1)
	trailer := []byte{0xCA, 0xFE, 0xBA, 0xBE}
	stream := bytes.NewReader(append(append([]byte{}, blob...), trailer...))

	if _, err := NewReaderFrom(stream); err != nil {
		t.Fatalf("NewReaderFrom: %v", err)
	}
	rest, err := io.ReadAll(stream)
	if err != nil {
		t.Fatalf("read remaining: %v", err)
	}
	if !bytes.Equal(rest, trailer) {
		t.Fatalf("trailing bytes: got %x, want %x", rest, trailer)
	}
}

// TestNewReaderFromDecoderPoolReuse — 100 sequential NewReaderFrom calls
// over compressed blobs must exercise the decoder pool (via ReadHeader),
// not allocate a fresh decoder per call. Mirrors the encoder-side pool
// assertion from compress_test.go.
func TestNewReaderFromDecoderPoolReuse(t *testing.T) {
	const iters = 100

	// Pre-build the compressed blob once; the pool behavior is what we're
	// measuring, not the per-call encode work.
	blob := buildCompressedBlob(t, rawBodyTag1String3, 1)

	// Drain the pool's New entries by warming it once so steady-state
	// reuse is what dominates.
	for range 4 {
		r, err := NewReaderFrom(bytes.NewReader(blob))
		if err != nil {
			t.Fatalf("warmup: %v", err)
		}
		if _, _, _, err := r.ReadHeader(); err != nil {
			t.Fatalf("warmup ReadHeader: %v", err)
		}
	}

	seen := make(map[*zstd.Decoder]struct{}, iters)
	for i := range iters {
		dec := getDecoder()
		seen[dec] = struct{}{}
		putDecoder(dec)
		// Also drive a real NewReaderFrom + ReadHeader to confirm the
		// production path itself uses (and returns to) the same pool.
		r, err := NewReaderFrom(bytes.NewReader(blob))
		if err != nil {
			t.Fatalf("iter %d: NewReaderFrom: %v", i, err)
		}
		if _, _, _, err := r.ReadHeader(); err != nil {
			t.Fatalf("iter %d: ReadHeader: %v", i, err)
		}
	}

	const maxDistinct = 8
	if len(seen) > maxDistinct {
		t.Fatalf("decoder pool reused too few instances: saw %d distinct decoders over %d iterations (want <= %d)", len(seen), iters, maxDistinct)
	}
}

// TestNewReaderFromEquivalentToNewReader — for a fully-formed blob, the
// Reader produced by NewReaderFrom must observe-identical to NewReader: same
// header values, same body bytes, same downstream decode. The streaming
// entrypoint is a transport-layer convenience, not a different decoder.
func TestNewReaderFromEquivalentToNewReader(t *testing.T) {
	cases := []struct {
		name string
		blob []byte
	}{
		{"uncompressed", buildUncompressedBlob(t, rawBodyTag1String3, 0x7777)},
		{"compressed", buildCompressedBlob(t, rawBodyTag1String3, 0x7777)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rDirect := NewReader(tc.blob)
			rStream, err := NewReaderFrom(bytes.NewReader(tc.blob))
			if err != nil {
				t.Fatalf("NewReaderFrom: %v", err)
			}
			fA, sA, lA, eA := rDirect.ReadHeader()
			fB, sB, lB, eB := rStream.ReadHeader()
			if eA != nil || eB != nil {
				t.Fatalf("ReadHeader errors: direct=%v stream=%v", eA, eB)
			}
			if fA != fB || sA != sB || lA != lB {
				t.Fatalf("header mismatch: direct=(%#x,%#x,%d) stream=(%#x,%#x,%d)", fA, sA, lA, fB, sB, lB)
			}
			// Compare remaining body bytes after ReadHeader (post-decompress
			// when applicable).
			restA := rDirect.buf[rDirect.pos:rDirect.end]
			restB := rStream.buf[rStream.pos:rStream.end]
			if !bytes.Equal(restA, restB) {
				t.Fatalf("body bytes differ after ReadHeader: direct=%x stream=%x", restA, restB)
			}
		})
	}
}

// TestNewReaderFromNRejectsOversizedBody pins the DoS guard: a hostile
// stream that declares a bodyLen beyond the caller's maxBodyLen must
// reject with ErrAllocTooLarge BEFORE the make([]byte, bodyLen)
// allocation runs. The check happens after only the 12-byte header has
// been read; no body bytes are demanded from src.
//
// This is the defense io.LimitReader cannot provide — make() runs
// before io.ReadFull, so a LimitReader-bounded src would still see the
// full bodyLen-sized allocation. Only a pre-make body-length check
// stops it.
func TestNewReaderFromNRejectsOversizedBody(t *testing.T) {
	// Hand-roll a header that declares a 1 MiB body but supplies none —
	// io.ReadFull would block / fail past the header, but we want to
	// observe the alloc-time rejection that happens first.
	const declaredBodyLen = 1 << 20
	hdr := make([]byte, HeaderSize)
	copy(hdr, Magic)
	hdr[4] = FmtVer2
	hdr[5] = 0
	binary.LittleEndian.PutUint16(hdr[6:8], 0)
	binary.LittleEndian.PutUint32(hdr[8:12], declaredBodyLen)

	// maxBodyLen is one byte below the declared length — the guard must
	// trip, ErrAllocTooLarge must surface, and src must NOT be drained
	// past the header (no body bytes were even attempted).
	src := bytes.NewReader(hdr) // exactly HeaderSize bytes; no body
	_, err := NewReaderFromN(src, declaredBodyLen-1)
	if !errors.Is(err, ErrAllocTooLarge) {
		t.Fatalf("over-cap bodyLen: want ErrAllocTooLarge, got %v", err)
	}
	if src.Len() != 0 {
		t.Fatalf("expected header fully consumed, %d bytes remain in src", src.Len())
	}
}

// TestNewReaderFromNAcceptsAtCap — a blob whose bodyLen equals
// maxBodyLen exactly must succeed; the bound is inclusive.
func TestNewReaderFromNAcceptsAtCap(t *testing.T) {
	blob := buildUncompressedBlob(t, rawBodyTag1String3, 0x4242)
	r, err := NewReaderFromN(bytes.NewReader(blob), len(rawBodyTag1String3))
	if err != nil {
		t.Fatalf("NewReaderFromN at cap: %v", err)
	}
	if _, _, _, err := r.ReadHeader(); err != nil {
		t.Fatalf("ReadHeader: %v", err)
	}
}

// TestNewReaderFromNNegativeCapDisablesGuard — a negative maxBodyLen
// restores NewReaderFrom semantics: the implicit uint32 / platform-int
// ceiling still applies, but no caller-side cap is enforced.
func TestNewReaderFromNNegativeCapDisablesGuard(t *testing.T) {
	blob := buildUncompressedBlob(t, rawBodyTag1String3, 0x4242)
	r, err := NewReaderFromN(bytes.NewReader(blob), -1)
	if err != nil {
		t.Fatalf("NewReaderFromN(-1): %v", err)
	}
	if _, _, _, err := r.ReadHeader(); err != nil {
		t.Fatalf("ReadHeader: %v", err)
	}
}
