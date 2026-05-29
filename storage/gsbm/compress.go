package gsbm

import (
	"bytes"
	"errors"
	"io"
	"math"
	"sync"

	"github.com/klauspost/compress/gzip"
	"github.com/klauspost/compress/zstd"
)

// zstdDecoderOverAllocSlack mirrors klauspost/compress/zstd's internal
// compressedBlockOverAlloc constant (16 bytes as of v1.18.6). DecodeAll's
// output-buffer growth path computes the new slice capacity as
// len(dst)+int(FrameContentSize)+compressedBlockOverAlloc, so the
// platform-int headroom we leave below math.MaxInt MUST cover this slack
// or the cap arithmetic overflows on 32-bit and make() panics.
const zstdDecoderOverAllocSlack = 16

// decoderMaxDecompressedSize caps the per-blob decompressed body size for
// both the zstd and gzip decode paths (zstd via the pooled decoder's
// WithDecoderMaxMemory, gzip via decompressGzip's explicit io.LimitReader
// cap). The ceiling is the smaller of the
// on-disk bodyLen field's uint32 maximum (~4 GiB) and a platform-int
// derived ceiling (math.MaxInt minus zstdDecoderOverAllocSlack; ~2 GiB on
// 32-bit, dominated by math.MaxUint32 on 64-bit). Without the platform-int
// clamp, a frame declaring a Frame_Content_Size between math.MaxInt and
// math.MaxUint32 would pass the WithDecoderMaxMemory check on a 32-bit
// build and then panic inside klauspost when it grows the output buffer
// to int(FrameContentSize)+compressedBlockOverAlloc — the +16 slack
// overflows int32. The subtraction of zstdDecoderOverAllocSlack keeps the
// cap arithmetic in-range and preserves the panic-free hostile-input rule
// from spec.md §8 on 32-bit targets the framing layer already guards in
// NewReaderFrom (reader.go:88). Without the uint32 clamp on 64-bit,
// klauspost's default WithDecoderMaxMemory is 64 GiB per DecodeAll call,
// so a small high-ratio frame ("zstd bomb") could OOM the process.
var decoderMaxDecompressedSize = func() uint64 {
	platformIntCap := uint64(math.MaxInt) - zstdDecoderOverAllocSlack
	if platformIntCap < uint64(math.MaxUint32) {
		return platformIntCap
	}
	return uint64(math.MaxUint32)
}()

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

// gzip encoder/decoder pools mirror the zstd pools. A gzip codec instance
// holds a flate window and token buffers — far smaller than zstd's lookup
// tables and worker buffers (~1.3 MiB vs ~10 MiB resident per instance in
// the repeatednested benchmark), which is the resident-footprint win that
// makes gzip the default codec. Encoders are configured at
// DefaultCompression — the level that gives gzip its best density (it
// compresses the structured-repetitive payloads this format targets
// slightly tighter than zstd SpeedFastest) in exchange for more CPU,
// the tradeoff a caller picks gzip for.

var gzipEncoderPool = sync.Pool{
	New: func() any {
		// nil writer keeps the encoder reusable: marshalCompressedStreaming
		// binds it via Reset(&compressed) on borrow. DefaultCompression is
		// always a valid level, so NewWriterLevel never errors here.
		enc, err := gzip.NewWriterLevel(nil, gzip.DefaultCompression)
		if err != nil {
			panic("gsbm: gzip encoder construction failed: " + err.Error())
		}
		return enc
	},
}

var gzipDecoderPool = sync.Pool{
	New: func() any {
		// A zero-value *gzip.Reader is safe to Reset (that is exactly how
		// gzip.NewReader bootstraps one), so the pool hands out bare
		// readers and decompressGzip binds each to its source via Reset.
		return new(gzip.Reader)
	},
}

// bodyCompressor is the slice of the codec API the streaming encode path
// needs. Both *zstd.Encoder and *gzip.Writer satisfy it, so
// marshalCompressedStreaming stays codec-agnostic: borrow via
// getCompressor, Reset to the output buffer, stream the body through
// Write, then Close to flush the final frame.
type bodyCompressor interface {
	io.Writer
	Reset(io.Writer)
	Close() error
}

// getCompressor borrows a pooled encoder for the named compressing codec.
// Callers MUST return it via putCompressor with the same method. method
// MUST be a compressing codec (CompressionZstd or CompressionGzip); the
// no-compression case never reaches here.
func getCompressor(method CompressionMethod) bodyCompressor {
	switch method {
	case CompressionZstd:
		return getEncoder()
	case CompressionGzip:
		return gzipEncoderPool.Get().(*gzip.Writer)
	default:
		panic("gsbm: getCompressor called with non-compressing method")
	}
}

// putCompressor returns c to the pool for method. It first unbinds the
// downstream writer (Reset(nil)) so a pooled codec never pins the caller's
// output buffer across pool entries — the same hygiene the zstd path
// relied on before generalization. Callers MUST have flushed and Closed
// the codec cleanly first; a codec left in an error state must be dropped
// (not returned) so the next borrower gets a fresh one.
func putCompressor(method CompressionMethod, c bodyCompressor) {
	c.Reset(nil)
	switch method {
	case CompressionZstd:
		putEncoder(c.(*zstd.Encoder))
	case CompressionGzip:
		gzipEncoderPool.Put(c.(*gzip.Writer))
	}
}

// decompressGzip inflates a gzip frame from src, enforcing the same
// inflated-size ceiling the zstd path gets for free from
// WithDecoderMaxMemory. klauspost's gzip.Reader has no max-memory option,
// so without this a tiny frame could inflate to gigabytes ("gzip bomb")
// and OOM the process — violating the panic-free / bounded hostile-input
// rule the framing layer already guarantees for zstd (spec.md §8). The
// reader is read through an io.LimitReader capped one byte above
// decoderMaxDecompressedSize; crossing that ceiling, or any read/header
// error, is reported as a decode failure for the caller to collapse into
// ErrCorruptCompressedBody.
func decompressGzip(src []byte) ([]byte, error) {
	gr := gzipDecoderPool.Get().(*gzip.Reader)
	defer gzipDecoderPool.Put(gr)
	if err := gr.Reset(bytes.NewReader(src)); err != nil {
		return nil, err
	}
	// decoderMaxDecompressedSize ≤ math.MaxUint32, so the +1 cannot
	// overflow int64 on any platform.
	limited := io.LimitReader(gr, int64(decoderMaxDecompressedSize)+1)
	out, err := io.ReadAll(limited)
	if err != nil {
		return nil, err
	}
	if uint64(len(out)) > decoderMaxDecompressedSize {
		return nil, errGzipTooLarge
	}
	return out, nil
}

// errGzipTooLarge marks a gzip frame whose inflated body exceeds
// decoderMaxDecompressedSize. It never escapes the package: ReadHeader
// collapses it into ErrCorruptCompressedBody alongside every other gzip
// decode failure.
var errGzipTooLarge = errors.New("gsbm: gzip inflated body exceeds decode cap")

// errUnreachableCodec guards the decompressBody default arm. It cannot
// fire in practice: ReadHeader only calls decompressBody for a method
// that headerCompressionMethod already validated as a known compressing
// codec.
var errUnreachableCodec = errors.New("gsbm: decompressBody called with non-compressing method")

// decompressBody inflates a compressed body with the codec named by
// method, both bounded by decoderMaxDecompressedSize (zstd via the
// pooled decoder's WithDecoderMaxMemory, gzip via decompressGzip's
// explicit cap). The returned slice is independent of any pooled codec
// state. Errors are returned raw for ReadHeader to collapse into
// ErrCorruptCompressedBody.
func decompressBody(method CompressionMethod, src []byte) ([]byte, error) {
	switch method {
	case CompressionZstd:
		dec := getDecoder()
		defer putDecoder(dec)
		return dec.DecodeAll(src, nil)
	case CompressionGzip:
		return decompressGzip(src)
	default:
		return nil, errUnreachableCodec
	}
}
