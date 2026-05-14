package gsbm

import (
	"math"
	"runtime"
)

// ProbeSamples captures two runtime.MemStats snapshots and two summary
// byte counts around a hand-rolled twin of gsbm.Marshal. Each MemStats
// sample is taken immediately after a synchronous runtime.GC() so it
// reflects live retention rather than dead-but-unswept bytes.
//
// Used by the peak-heap benchmark in gsbm_test to demonstrate the
// streaming-vs-materializing-cached trade-off (issue #30):
//
//   - Cached: at Mid the output buffer is freshly allocated (~1× blob)
//     and the scratch cache holds every occurrence's materialized JSON
//     (~1× blob). Both are live simultaneously → ~2× blob.
//   - Streaming: at Mid the output buffer is freshly allocated (~1×
//     blob) and no scratch cache exists. → ~1× blob.
//
// Late is sampled after the write pass returns; for cached the scratch
// remains rooted alongside the now-filled output buffer, but allocator
// size-class rounding plus released transient grows often shrink the
// observed total below the Mid peak. Peak is therefore max(Mid, Late).
type ProbeSamples struct {
	// Before is the heap baseline immediately before the size pass
	// starts, after a synchronous GC. Used to subtract fixture and
	// runtime resident bytes from peak deltas.
	Before runtime.MemStats
	// Mid is sampled after the size pass completes, the output buffer
	// is allocated, and the scratch is transferred via adoptScratch,
	// but before the write pass begins.
	Mid runtime.MemStats
	// Late is sampled after the write pass returns.
	Late runtime.MemStats
	// ScratchBytes is the sum of byte capacities held by the scratch
	// cache at the end of the write pass — the bytes the cached kind
	// retains alongside the output buffer.
	ScratchBytes uint64
	// OutputBytes is the capacity of the output buffer at the end of
	// the write pass.
	OutputBytes uint64
}

// MarshalWithProbe is a test-only twin of Marshal that hand-rolls the
// two-pass encode and captures ProbeSamples at the boundary between the
// size and write passes (with the output buffer allocated and the
// scratch already transferred) and immediately after the write pass
// returns. The wire output is byte-identical to Marshal(v, schemaHint).
//
// Exported for the gsbm_test package's peak-heap benchmark, which lives
// outside this package and cannot call the unexported adoptScratch
// directly. Production code uses Marshal.
func MarshalWithProbe(v Marshaler, schemaHint uint16) ([]byte, ProbeSamples, error) {
	var samples ProbeSamples
	runtime.GC()
	runtime.ReadMemStats(&samples.Before)

	sw := NewCountingWriter()
	if err := v.MarshalGSBM(sw); err != nil {
		return nil, samples, err
	}
	if err := sw.Err(); err != nil {
		return nil, samples, err
	}
	bodyLen := sw.Size()
	if bodyLen < 0 || uint64(bodyLen) > math.MaxUint32 {
		return nil, samples, ErrBodyTooLarge
	}

	bw := NewWriter(make([]byte, 0, HeaderSize+bodyLen))
	bw.adoptScratch(sw)

	runtime.GC()
	runtime.ReadMemStats(&samples.Mid)

	bw.WriteHeader(0, schemaHint, uint32(bodyLen))
	if err := v.MarshalGSBM(bw); err != nil {
		return nil, samples, err
	}
	if err := bw.Err(); err != nil {
		return nil, samples, err
	}

	runtime.GC()
	runtime.ReadMemStats(&samples.Late)

	for _, e := range bw.scratch {
		samples.ScratchBytes += e.totalBytes()
	}
	samples.OutputBytes = uint64(cap(bw.buf))

	return bw.Bytes(), samples, nil
}

func (e *scratchEntry) totalBytes() uint64 {
	var n uint64
	for _, s := range e.strings {
		n += uint64(len(s))
	}
	for _, v := range e.values {
		n += uint64(cap(v))
	}
	return n
}
