package gsbm

import (
	"math"
	"testing"

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
