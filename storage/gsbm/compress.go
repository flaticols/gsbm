package gsbm

import (
	"math"
	"sync"

	"github.com/klauspost/compress/zstd"
)

// decoderMaxDecompressedSize caps the per-blob decompressed body size the
// pooled zstd decoder will produce. It matches the on-disk bodyLen field's
// uint32 ceiling so the inflated-output bound is symmetric with the
// compressed-input bound — a blob whose compressed body fits the wire
// format can never inflate beyond the same 4 GiB ceiling. Without this
// cap, klauspost's default WithDecoderMaxMemory is 64 GiB per DecodeAll
// call, so a small high-ratio frame ("zstd bomb") could OOM the process
// in violation of the panic-free hostile-input rule in spec.md §8.
const decoderMaxDecompressedSize = uint64(math.MaxUint32)

// zstd encoder/decoder construction is expensive (per-instance lookup tables
// and worker setup); the framing layer reuses both via sync.Pool so that
// repeated MarshalWithOptions / NewReader calls amortize the cost. Encoders
// are configured at SpeedFastest — the codec choice the spec settles on for
// the structured-repetitive payloads this format targets.

var encoderPool = sync.Pool{
	New: func() any {
		// nil writer keeps the encoder reusable: callers either Reset(w)
		// for streaming or EncodeAll(src, dst) for the buffered path.
		// Concurrency is pinned at 1 to keep the streaming write path's
		// per-call allocations bounded. The default (GOMAXPROCS) spawns
		// worker goroutines that each maintain their own block buffers;
		// for a single-payload encode that's pure overhead and pushes
		// per-call TotalAlloc well past the raw body size, breaking the
		// "raw body never lands in any single buffer" guarantee
		// MarshalToWriter is contracted to provide. Throughput across
		// many concurrent encodes is recovered at the pool layer, not
		// inside one encoder.
		enc, err := zstd.NewWriter(nil,
			zstd.WithEncoderLevel(zstd.SpeedFastest),
			zstd.WithEncoderConcurrency(1),
		)
		if err != nil {
			// zstd.NewWriter only errors on bad options; SpeedFastest is
			// always valid, so this is a build-time bug if it fires.
			panic("gsbm: zstd encoder construction failed: " + err.Error())
		}
		return enc
	},
}

var decoderPool = sync.Pool{
	New: func() any {
		// Concurrency is pinned at 1 for symmetry with the encoder: the
		// default (GOMAXPROCS) spawns a worker goroutine per pool entry
		// that survives sync.Pool victim-cache eviction, accumulating
		// dormant goroutines over the lifetime of a long-running process.
		// DecodeAll is a one-shot call shape that has no use for worker
		// parallelism anyway.
		dec, err := zstd.NewReader(nil,
			zstd.WithDecoderConcurrency(1),
			zstd.WithDecoderMaxMemory(decoderMaxDecompressedSize),
		)
		if err != nil {
			panic("gsbm: zstd decoder construction failed: " + err.Error())
		}
		return dec
	},
}

// getEncoder borrows a pooled zstd encoder. Callers must return it via
// putEncoder when done. The encoder is not bound to any writer; bind via
// Reset(w) for streaming or use EncodeAll(src, dst) for the buffered path.
func getEncoder() *zstd.Encoder {
	return encoderPool.Get().(*zstd.Encoder)
}

// putEncoder returns enc to the pool. Callers must have flushed/closed any
// in-flight stream first (Close or Reset(nil)) so the next borrower sees a
// clean encoder.
func putEncoder(enc *zstd.Encoder) {
	encoderPool.Put(enc)
}

// getDecoder borrows a pooled zstd decoder. Callers must return it via
// putDecoder when done.
func getDecoder() *zstd.Decoder {
	return decoderPool.Get().(*zstd.Decoder)
}

// putDecoder returns dec to the pool. Callers must have drained or reset
// the decoder first.
func putDecoder(dec *zstd.Decoder) {
	decoderPool.Put(dec)
}
