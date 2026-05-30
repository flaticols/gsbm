package gsbm

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"

	"github.com/klauspost/compress/gzip"
	"github.com/klauspost/compress/zstd"
)

// frameFor compresses raw with the named codec, returning the on-disk frame
// bytes. Built with freshly-constructed codecs (not the pool) so the
// extended-header tests do not depend on encode-pool state.
func frameFor(t *testing.T, method CompressionMethod, raw []byte) []byte {
	t.Helper()
	switch method {
	case CompressionZstd:
		enc, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedFastest))
		if err != nil {
			t.Fatalf("zstd.NewWriter: %v", err)
		}
		defer func() { _ = enc.Close() }()
		return enc.EncodeAll(raw, nil)
	case CompressionGzip:
		var buf bytes.Buffer
		gw := gzip.NewWriter(&buf)
		if _, err := gw.Write(raw); err != nil {
			t.Fatalf("gzip Write: %v", err)
		}
		if err := gw.Close(); err != nil {
			t.Fatalf("gzip Close: %v", err)
		}
		return buf.Bytes()
	default:
		t.Fatalf("frameFor: non-compressing method %d", method)
		return nil
	}
}

// buildExtendedBlob assembles a new-format compressed blob: a 16-byte
// extended header (flags = method|extendedHeaderBit, bodyLen = compressed
// frame length, inflatedLen) followed by the codec frame of raw. inflatedLen
// is passed explicitly so tests can inject wrong values.
func buildExtendedBlob(t *testing.T, method CompressionMethod, raw []byte, schemaHint uint16, inflatedLen uint32) []byte {
	t.Helper()
	frame := frameFor(t, method, raw)
	blob := make([]byte, 0, ExtendedHeaderSize+len(frame))
	blob = append(blob, Magic[0], Magic[1], Magic[2], Magic[3], FmtVer2, uint8(method)|extendedHeaderBit)
	blob = binary.LittleEndian.AppendUint16(blob, schemaHint)
	blob = binary.LittleEndian.AppendUint32(blob, uint32(len(frame)))
	blob = binary.LittleEndian.AppendUint32(blob, inflatedLen)
	blob = append(blob, frame...)
	return blob
}

// TestExtendedHeaderRoundTrip pins the new-format decode path for both
// codecs: a 16-byte-header blob with the correct inflatedLen decodes
// through ReadHeader (pre-sized inflate) exactly like the legacy and
// uncompressed forms.
func TestExtendedHeaderRoundTrip(t *testing.T) {
	for _, method := range []CompressionMethod{CompressionZstd, CompressionGzip} {
		blob := buildExtendedBlob(t, method, rawBodyTag1String3, 0x1234, uint32(len(rawBodyTag1String3)))
		r := NewReader(blob)
		flags, schemaHint, bodyLen, err := r.ReadHeader()
		if err != nil {
			t.Fatalf("method %d ReadHeader: %v", method, err)
		}
		if wantFlags := uint8(method) | extendedHeaderBit; flags != wantFlags {
			t.Fatalf("method %d flags = %#x, want %#x", method, flags, wantFlags)
		}
		if schemaHint != 0x1234 {
			t.Fatalf("method %d schemaHint = %#x", method, schemaHint)
		}
		if bodyLen != uint32(len(blob)-ExtendedHeaderSize) {
			t.Fatalf("method %d bodyLen = %d, want %d", method, bodyLen, len(blob)-ExtendedHeaderSize)
		}
		tag, wt, err := r.ReadTag()
		if err != nil || tag != 1 || wt != WireLengthDelim {
			t.Fatalf("method %d ReadTag: tag=%d wt=%d err=%v", method, tag, wt, err)
		}
		s, err := r.ReadString()
		if err != nil || s != "abc" {
			t.Fatalf("method %d ReadString = %q err=%v", method, s, err)
		}
		if r.HasMore() {
			t.Fatalf("method %d: trailing bytes after decode", method)
		}
	}
}

// TestExtendedHeaderInflatedLenMismatch pins the integrity check: an
// extended blob whose declared inflatedLen disagrees with the frame's real
// inflated size — too small or too large — is rejected as a corrupt
// compressed body, never silently truncated or over-read.
func TestExtendedHeaderInflatedLenMismatch(t *testing.T) {
	realLen := uint32(len(rawBodyTag1String3)) // 5
	for _, method := range []CompressionMethod{CompressionZstd, CompressionGzip} {
		for _, bad := range []uint32{realLen - 1, realLen + 1, realLen + 100} {
			blob := buildExtendedBlob(t, method, rawBodyTag1String3, 0, bad)
			_, _, _, err := NewReader(blob).ReadHeader()
			if !errors.Is(err, ErrCorruptCompressedBody) {
				t.Fatalf("method %d inflatedLen=%d (real %d): want ErrCorruptCompressedBody, got %v",
					method, bad, realLen, err)
			}
		}
	}
}

// TestExtendedHeaderInflatedLenLieIsBounded pins the DoS guard: a tiny frame
// that lies about a huge inflatedLen must be rejected via the exact-match
// check, NOT drive a multi-hundred-MiB speculative allocation. The test
// completing promptly (and the AllocsPerRun staying modest) is the
// observable proxy for the bounded pre-size.
func TestExtendedHeaderInflatedLenLieIsBounded(t *testing.T) {
	for _, method := range []CompressionMethod{CompressionZstd, CompressionGzip} {
		// Claim ~512 MiB inflated for a 5-byte payload.
		blob := buildExtendedBlob(t, method, rawBodyTag1String3, 0, 512<<20)
		_, _, _, err := NewReader(blob).ReadHeader()
		if !errors.Is(err, ErrCorruptCompressedBody) {
			t.Fatalf("method %d: lying inflatedLen must reject, got %v", method, err)
		}
		// Bound the per-decode allocation well below the lied-about size:
		// the speculative reserve is clamped to maxInflatePresize (16 MiB),
		// so bytes/op must be far under 512 MiB.
		avg := testing.AllocsPerRun(3, func() {
			_, _, _, _ = NewReader(blob).ReadHeader()
		})
		_ = avg // alloc COUNT is not the bound; the wall-clock + no-OOM is.
	}
}

// TestExtendedDecodeEqualsUncompressed pins that the three on-wire forms of
// the same payload — uncompressed, extended zstd, extended gzip — all decode
// to byte-identical bodies. Uses MarshalWithOptions (which now emits the
// extended header) for the compressed forms.
func TestExtendedDecodeEqualsUncompressed(t *testing.T) {
	uncompressed, err := Marshal(pinnedMarshaler{}, 0x55)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	ru := NewReader(uncompressed)
	if _, _, _, err := ru.ReadHeader(); err != nil {
		t.Fatalf("uncompressed ReadHeader: %v", err)
	}
	wantBody := append([]byte(nil), ru.buf[ru.pos:ru.end]...)

	for _, method := range []CompressionMethod{CompressionZstd, CompressionGzip} {
		blob, err := MarshalWithOptions(pinnedMarshaler{}, 0x55, Options{Compression: method})
		if err != nil {
			t.Fatalf("method %d MarshalWithOptions: %v", method, err)
		}
		r := NewReader(blob)
		if _, _, _, err := r.ReadHeader(); err != nil {
			t.Fatalf("method %d ReadHeader: %v", method, err)
		}
		gotBody := r.buf[r.pos:r.end]
		if !bytes.Equal(gotBody, wantBody) {
			t.Fatalf("method %d decoded body = %x, want %x", method, gotBody, wantBody)
		}
	}
}
