package gsbm

import (
	"bytes"
	"math"
	"testing"

	"github.com/klauspost/compress/gzip"
	"github.com/klauspost/compress/zstd"
)

// TestDecoderMaxDecompressedSizeLeavesOverAllocHeadroom pins the 32-bit
// boundary fix for the decoder cap. klauspost/compress/zstd's DecodeAll
// grows its output buffer to int(FrameContentSize)+compressedBlockOverAlloc;
// if our cap equals math.MaxInt the +16 slack overflows int32 on a 32-bit
// build and make() panics instead of returning ErrCorruptCompressedBody.
// The cap must stay at least zstdDecoderOverAllocSlack below math.MaxInt
// so the cap arithmetic is in-range on every supported platform.
func TestDecoderMaxDecompressedSizeLeavesOverAllocHeadroom(t *testing.T) {
	limit := uint64(math.MaxInt) - zstdDecoderOverAllocSlack
	if decoderMaxDecompressedSize > limit {
		t.Fatalf("decoderMaxDecompressedSize=%d exceeds math.MaxInt-%d=%d; int(FCS)+%d would overflow on 32-bit",
			decoderMaxDecompressedSize, zstdDecoderOverAllocSlack, limit, zstdDecoderOverAllocSlack)
	}
}

// TestCompressEncoderPoolReuse asserts that the encoder pool actually
// recycles instances across get/put cycles. zstd encoder construction is
// expensive (per-instance lookup tables); the framing layer's compression
// path is only viable if pooled encoders are reused, not re-constructed on
// every Marshal call. The pool guarantees nothing about *which* instance
// any single Get returns, but a tight serial loop of paired Get/Put must
// observe far fewer distinct instances than iterations.
func TestCompressEncoderPoolReuse(t *testing.T) {
	const iters = 100

	seen := make(map[*zstd.Encoder]struct{}, iters)
	for i := range iters {
		enc := getEncoder()
		if enc == nil {
			t.Fatalf("iter %d: getEncoder returned nil", i)
		}
		seen[enc] = struct{}{}
		putEncoder(enc)
	}

	// Tight serial Get/Put with no concurrency: one cached instance is the
	// expected steady state. Allow a small slack for GC eviction between
	// iterations (sync.Pool may drop entries on GC); 8 is generous and
	// still catches the "no reuse at all" regression where seen == iters.
	const maxDistinct = 8
	if len(seen) > maxDistinct {
		t.Fatalf("encoder pool reused too few instances: saw %d distinct encoders over %d iterations (want <= %d)", len(seen), iters, maxDistinct)
	}
}

// TestCompressDecoderPoolReuse mirrors the encoder test for the decoder pool.
func TestCompressDecoderPoolReuse(t *testing.T) {
	const iters = 100

	seen := make(map[*zstd.Decoder]struct{}, iters)
	for i := range iters {
		dec := getDecoder()
		if dec == nil {
			t.Fatalf("iter %d: getDecoder returned nil", i)
		}
		seen[dec] = struct{}{}
		putDecoder(dec)
	}

	const maxDistinct = 8
	if len(seen) > maxDistinct {
		t.Fatalf("decoder pool reused too few instances: saw %d distinct decoders over %d iterations (want <= %d)", len(seen), iters, maxDistinct)
	}
}

// TestGzipEncoderPoolReuse mirrors TestCompressEncoderPoolReuse for the
// gzip writer pool reached through the getCompressor/putCompressor
// dispatchers. A tight serial Get/Put loop must observe far fewer distinct
// instances than iterations, or the gzip path re-constructs an encoder on
// every Marshal — the regression the pool exists to prevent.
func TestGzipEncoderPoolReuse(t *testing.T) {
	const iters = 100
	seen := make(map[*gzip.Writer]struct{}, iters)
	for i := range iters {
		c := getCompressor(CompressionGzip)
		gw, ok := c.(*gzip.Writer)
		if !ok {
			t.Fatalf("iter %d: getCompressor(gzip) returned %T, want *gzip.Writer", i, c)
		}
		seen[gw] = struct{}{}
		putCompressor(CompressionGzip, c)
	}
	const maxDistinct = 8
	if len(seen) > maxDistinct {
		t.Fatalf("gzip encoder pool reused too few instances: saw %d distinct writers over %d iterations (want <= %d)", len(seen), iters, maxDistinct)
	}
}

// TestCompressorDispatchRoundTrip exercises the production codec
// dispatchers end-to-end: getCompressor → Reset/Write/Close →
// putCompressor on the encode side, decompressBody on the decode side,
// for each compressing method. It pins that the bodyCompressor interface
// abstraction and the pool reset hygiene round-trip arbitrary bytes.
func TestCompressorDispatchRoundTrip(t *testing.T) {
	raw := bytes.Repeat([]byte("the quick brown fox jumps over 13 lazy dogs; "), 64)
	for _, m := range []CompressionMethod{CompressionZstd, CompressionGzip} {
		c := getCompressor(m)
		var buf bytes.Buffer
		c.Reset(&buf)
		if _, err := c.Write(raw); err != nil {
			t.Fatalf("method %d Write: %v", m, err)
		}
		if err := c.Close(); err != nil {
			t.Fatalf("method %d Close: %v", m, err)
		}
		putCompressor(m, c)

		out, err := decompressBody(m, buf.Bytes())
		if err != nil {
			t.Fatalf("method %d decompressBody: %v", m, err)
		}
		if !bytes.Equal(out, raw) {
			t.Fatalf("method %d round-trip mismatch: got %d bytes, want %d", m, len(out), len(raw))
		}
	}
}

// TestGetCompressorPanicsOnNoneMethod pins the internal contract that the
// dispatcher is never called for a non-compressing method — callers route
// CompressionNone to the uncompressed path. A regression that lets None
// reach getCompressor would otherwise write a codec-less frame.
func TestGetCompressorPanicsOnNoneMethod(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("getCompressor(CompressionNone) did not panic")
		}
	}()
	_ = getCompressor(CompressionNone)
}
