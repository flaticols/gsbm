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

// capLimitedDecoderPool mirrors decoderPool but adds
// WithDecodeAllCapLimit(true): DecodeAll on these decoders treats the
// destination's spare capacity as a HARD limit and rejects a frame whose
// declared FrameContentSize exceeds it BEFORE allocating. The extended
// (known-inflatedLen) path uses this so it can hand DecodeAll a buffer
// sized to inflatedLen and get the tight, single-shot allocation of
// DecodeAll while a frame that lies about a huge FCS is rejected without
// driving a giant output-buffer growth. The plain decoderPool keeps the
// no-limit behavior the legacy (unknown-length, dst=nil) path needs.
var capLimitedDecoderPool = sync.Pool{
	New: func() any {
		dec, err := zstd.NewReader(nil,
			zstd.WithDecoderConcurrency(1),
			zstd.WithDecoderMaxMemory(decoderMaxDecompressedSize),
			zstd.WithDecodeAllCapLimit(true),
		)
		if err != nil {
			panic("gsbm: zstd cap-limited decoder construction failed: " + err.Error())
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

// getCapLimitedDecoder / putCapLimitedDecoder borrow and return a decoder
// from the WithDecodeAllCapLimit pool (see capLimitedDecoderPool).
func getCapLimitedDecoder() *zstd.Decoder {
	return capLimitedDecoderPool.Get().(*zstd.Decoder)
}

func putCapLimitedDecoder(dec *zstd.Decoder) {
	capLimitedDecoderPool.Put(dec)
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
// inflatedLen carries the exact decompressed body byte count when it is
// known from an extended header (flags bit 3), or unknownInflatedLen when
// it is not (uncompressed-path callers and legacy compressed blobs that
// predate the extended header). A known length lets the decode path
// pre-size its output buffer and bound the inflate to exactly that many
// bytes — a per-blob hard cap tighter than the global
// decoderMaxDecompressedSize ceiling.
const unknownInflatedLen = -1

// maxInflatePresize bounds how many bytes the decode path will
// speculatively reserve from an attacker-controlled inflatedLen before any
// bytes are actually inflated. A blob declaring a larger inflatedLen still
// decodes — the buffer grows from this ceiling as real bytes arrive — but a
// tiny "the header lied" frame cannot force a multi-GiB up-front
// allocation. Bodies at or below this size (the common case for stored
// records) reserve exactly inflatedLen and so avoid the decompressor's
// geometric over-growth entirely. The absolute inflate bound remains
// decoderMaxDecompressedSize, enforced per codec below.
const maxInflatePresize = 16 << 20 // 16 MiB

// presizeFor clamps a known inflatedLen to the speculative-allocation
// ceiling. The returned value is always in [0, maxInflatePresize].
func presizeFor(inflatedLen int) int {
	if inflatedLen > maxInflatePresize {
		return maxInflatePresize
	}
	if inflatedLen < 0 {
		return 0
	}
	return inflatedLen
}

func decompressGzip(src []byte, inflatedLen int) ([]byte, error) {
	gr := gzipDecoderPool.Get().(*gzip.Reader)
	defer gzipDecoderPool.Put(gr)
	if err := gr.Reset(bytes.NewReader(src)); err != nil {
		return nil, err
	}
	if inflatedLen >= 0 {
		// Known inflated size: reserve up to the speculative ceiling (so a
		// hostile huge inflatedLen cannot drive a giant up-front alloc) and
		// bound the inflate at inflatedLen+1 so a frame that inflates beyond
		// the declared size — a writer bug or a "the header lied" frame — is
		// rejected, not over-read. A frame that inflates short is rejected
		// too: the decoded length must match inflatedLen exactly.
		var buf bytes.Buffer
		buf.Grow(presizeFor(inflatedLen))
		n, err := buf.ReadFrom(io.LimitReader(gr, int64(inflatedLen)+1))
		if err != nil {
			return nil, err
		}
		if n != int64(inflatedLen) {
			return nil, errInflatedLenMismatch
		}
		return buf.Bytes(), nil
	}
	// Unknown inflated size (legacy compressed blob): inflate through an
	// io.LimitReader capped one byte above the global ceiling, exactly as
	// before the extended header existed.
	//
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

// errInflatedLenMismatch marks a compressed body (extended header, flags
// bit 3) whose actual inflated length disagrees with the header's
// inflatedLen field — a truncated/over-inflated frame or a writer that
// recorded the wrong size. Like errGzipTooLarge it never escapes the
// package: ReadHeader collapses it into ErrCorruptCompressedBody.
var errInflatedLenMismatch = errors.New("gsbm: inflated body length does not match header inflatedLen")

// errUnreachableCodec guards the decompressBody default arm. It cannot
// fire in practice: ReadHeader only calls decompressBody for a method
// that headerCompressionMethod already validated as a known compressing
// codec.
var errUnreachableCodec = errors.New("gsbm: decompressBody called with non-compressing method")

// decompressBody inflates a compressed body with the codec named by
// method. inflatedLen is the exact decompressed body size when known from
// an extended header (flags bit 3), or unknownInflatedLen otherwise.
//
// When inflatedLen is known, the body is stream-decoded with the read
// bounded at inflatedLen+1 and the speculative reservation clamped to the
// ceiling, then the decoded length is cross-checked against inflatedLen
// (mismatch ⇒ errInflatedLenMismatch). Streaming — rather than DecodeAll —
// is load-bearing for zstd: DecodeAll honors the frame's declared
// FrameContentSize for its output allocation even when handed a small dst,
// so a frame (or an extended header) that lies about a multi-GiB size would
// drive a huge up-front allocation before the length check could reject it.
// The streaming reader grows with the REAL inflated content instead, so a
// lying tiny frame is rejected at the ceiling while an honest large body
// still decodes (bounded by the decoder's WithDecoderMaxMemory). gzip is
// likewise bounded at inflatedLen+1 in decompressGzip.
//
// When inflatedLen is unknown (legacy compressed blob), behavior is
// identical to before the extended header existed: zstd via DecodeAll
// bounded by WithDecoderMaxMemory, gzip via the global inflate cap. The
// returned slice is independent of any pooled codec state. Errors are
// returned raw for ReadHeader to collapse into ErrCorruptCompressedBody.
func decompressBody(method CompressionMethod, src []byte, inflatedLen int) ([]byte, error) {
	switch method {
	case CompressionZstd:
		if inflatedLen >= 0 {
			return decompressZstdKnown(src, inflatedLen)
		}
		dec := getDecoder()
		defer putDecoder(dec)
		return dec.DecodeAll(src, nil)
	case CompressionGzip:
		return decompressGzip(src, inflatedLen)
	default:
		return nil, errUnreachableCodec
	}
}

// decompressZstdKnown inflates a zstd frame whose exact inflated size is
// known (extended header). For the common case — inflatedLen at or under
// the speculative ceiling — it hands a cap-limited DecodeAll a buffer sized
// to inflatedLen: DecodeAll fills it in one shot with no geometric
// over-growth (the tight allocation that removes the compressed-decode
// transient) and rejects any frame whose declared FrameContentSize exceeds
// the cap before allocating (the FCS-bomb guard). For the rare body larger
// than the ceiling, it streams with the read bounded at inflatedLen+1 so a
// huge honest body still decodes while a lying header is bounded by the
// ceiling reservation. Either way the decoded length must equal inflatedLen
// exactly.
func decompressZstdKnown(src []byte, inflatedLen int) ([]byte, error) {
	if inflatedLen <= maxInflatePresize {
		dec := getCapLimitedDecoder()
		defer putCapLimitedDecoder(dec)
		out, err := dec.DecodeAll(src, make([]byte, 0, inflatedLen))
		if err != nil {
			return nil, err
		}
		if len(out) != inflatedLen {
			return nil, errInflatedLenMismatch
		}
		return out, nil
	}
	// Large body: stream so the output buffer grows with real inflated
	// content rather than a (possibly lying) declared size. The extra
	// per-call decoder allocation the streaming path carries is amortized
	// over a body that is, by definition here, larger than the ceiling.
	dec := getDecoder()
	defer putDecoder(dec)
	if err := dec.Reset(bytes.NewReader(src)); err != nil {
		return nil, err
	}
	// Unbind the pooled decoder from src before it returns to the pool, so
	// it never pins the caller's bytes across borrows.
	defer func() { _ = dec.Reset(nil) }()
	var buf bytes.Buffer
	buf.Grow(maxInflatePresize)
	n, err := buf.ReadFrom(io.LimitReader(dec, int64(inflatedLen)+1))
	if err != nil {
		return nil, err
	}
	if n != int64(inflatedLen) {
		return nil, errInflatedLenMismatch
	}
	return buf.Bytes(), nil
}
