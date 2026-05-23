package gsbm

import (
	"bytes"
	"errors"
	"io"
	"runtime"
	"testing"

	"github.com/klauspost/compress/zstd"
)

// TestMarshalToWriterUncompressedRoundTrip exercises the buffered
// fallback for the uncompressed streaming path: MarshalToWriter with
// Options{} must produce the same bytes as Marshal and round-trip
// through NewReader.
func TestMarshalToWriterUncompressedRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	if err := MarshalToWriter(&buf, pinnedMarshaler{}, 0x1234, Options{}); err != nil {
		t.Fatalf("MarshalToWriter: %v", err)
	}
	want, err := Marshal(pinnedMarshaler{}, 0x1234)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !bytes.Equal(buf.Bytes(), want) {
		t.Fatalf("MarshalToWriter (uncompressed) bytes differ from Marshal:\n got %x\nwant %x", buf.Bytes(), want)
	}
}

// TestMarshalToWriterCompressedRoundTrip confirms the streaming
// compressed encode produces a blob that NewReader+ReadHeader+primitive
// reads will decode back to the original fields. Compression that can't
// round-trip is write-only.
func TestMarshalToWriterCompressedRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	if err := MarshalToWriter(&buf, pinnedMarshaler{}, 0xBEEF, Options{Compress: true}); err != nil {
		t.Fatalf("MarshalToWriter: %v", err)
	}

	r := NewReader(buf.Bytes())
	flags, hint, _, err := r.ReadHeader()
	if err != nil {
		t.Fatalf("ReadHeader: %v", err)
	}
	if flags != FlagCompressed {
		t.Fatalf("flags = %#x, want %#x", flags, FlagCompressed)
	}
	if hint != 0xBEEF {
		t.Fatalf("schemaHint = %#x, want 0xBEEF", hint)
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

// TestMarshalToWriterByteIdenticalToMarshalWithOptions asserts that the
// two entry points produce byte-for-byte identical blobs for the same
// input and options. This is the load-bearing equivalence property that
// lets callers freely switch between buffered ([]byte) and streaming
// (io.Writer) outputs without observable wire-format diffs.
func TestMarshalToWriterByteIdenticalToMarshalWithOptions(t *testing.T) {
	type tc struct {
		name string
		opts Options
	}
	cases := []tc{
		{"uncompressed", Options{}},
		{"compressed", Options{Compress: true}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a, err := MarshalWithOptions(pinnedMarshaler{}, 0x4242, c.opts)
			if err != nil {
				t.Fatalf("MarshalWithOptions: %v", err)
			}
			var buf bytes.Buffer
			if err := MarshalToWriter(&buf, pinnedMarshaler{}, 0x4242, c.opts); err != nil {
				t.Fatalf("MarshalToWriter: %v", err)
			}
			if !bytes.Equal(a, buf.Bytes()) {
				t.Fatalf("byte diff:\n MarshalWithOptions=%x\n MarshalToWriter   =%x", a, buf.Bytes())
			}
		})
	}
}

// TestMarshalToWriterCompressedSmallerThanUncompressed sanity-checks the
// compressed path produces a smaller blob than the uncompressed path on
// a payload with deliberate repetition. Catches a regression where the
// encoder is constructed without SpeedFastest, or where the streaming
// flush dumps raw bytes through unchanged.
func TestMarshalToWriterCompressedSmallerThanUncompressed(t *testing.T) {
	m := nestedRepeatedMarshaler{n: 200}

	var raw, compressed bytes.Buffer
	if err := MarshalToWriter(&raw, &m, 0, Options{}); err != nil {
		t.Fatalf("uncompressed MarshalToWriter: %v", err)
	}
	if err := MarshalToWriter(&compressed, &m, 0, Options{Compress: true}); err != nil {
		t.Fatalf("compressed MarshalToWriter: %v", err)
	}
	if compressed.Len() >= raw.Len() {
		t.Fatalf("compressed=%d bytes, uncompressed=%d bytes — compression did not shrink the body", compressed.Len(), raw.Len())
	}
}

// TestMarshalToWriterCompressedNestedRoundTrip drives the
// repeated-nested-style fixture so the streaming write pass exercises
// many sibling and nested BeginLengthDelim regions. The recorded-sizes
// machinery is what makes streaming correct here — a bug in the order
// or count of recorded sizes would surface as either a decode error or
// a body-bytes mismatch.
func TestMarshalToWriterCompressedNestedRoundTrip(t *testing.T) {
	m := nestedRepeatedMarshaler{n: 32}

	var buf bytes.Buffer
	if err := MarshalToWriter(&buf, &m, 0x0F0F, Options{Compress: true}); err != nil {
		t.Fatalf("MarshalToWriter: %v", err)
	}

	r := NewReader(buf.Bytes())
	flags, _, _, err := r.ReadHeader()
	if err != nil {
		t.Fatalf("ReadHeader: %v", err)
	}
	if flags != FlagCompressed {
		t.Fatalf("flags=%#x, want %#x", flags, FlagCompressed)
	}

	tag, wt, err := r.ReadTag()
	if err != nil {
		t.Fatalf("ReadTag: %v", err)
	}
	if tag != 1 || wt != WireLengthDelim {
		t.Fatalf("outer tag/wt = %d/%d, want 1/WireLengthDelim", tag, wt)
	}
	saved, err := r.BeginLengthDelim()
	if err != nil {
		t.Fatalf("BeginLengthDelim outer: %v", err)
	}
	n, err := r.ReadLength()
	if err != nil {
		t.Fatalf("ReadLength count: %v", err)
	}
	if n != m.n {
		t.Fatalf("inner count = %d, want %d", n, m.n)
	}
	for i := range n {
		inner, err := r.BeginLengthDelim()
		if err != nil {
			t.Fatalf("BeginLengthDelim inner #%d: %v", i, err)
		}
		// Each inner region is two tagged fields: tag=1 string and tag=2 varint.
		tag, wt, err := r.ReadTag()
		if err != nil {
			t.Fatalf("inner #%d tag1: %v", i, err)
		}
		if tag != 1 || wt != WireLengthDelim {
			t.Fatalf("inner #%d tag1: tag=%d wt=%d", i, tag, wt)
		}
		s, err := r.ReadString()
		if err != nil {
			t.Fatalf("inner #%d str: %v", i, err)
		}
		if s != "code" {
			t.Fatalf("inner #%d str=%q, want %q", i, s, "code")
		}
		tag, wt, err = r.ReadTag()
		if err != nil {
			t.Fatalf("inner #%d tag2: %v", i, err)
		}
		if tag != 2 || wt != WireVarint {
			t.Fatalf("inner #%d tag2: tag=%d wt=%d", i, tag, wt)
		}
		v, err := r.ReadUvarint()
		if err != nil {
			t.Fatalf("inner #%d val: %v", i, err)
		}
		if v != uint64(i) {
			t.Fatalf("inner #%d val=%d, want %d", i, v, i)
		}
		if err := r.EndLengthDelim(inner); err != nil {
			t.Fatalf("EndLengthDelim inner #%d: %v", i, err)
		}
	}
	if err := r.EndLengthDelim(saved); err != nil {
		t.Fatalf("EndLengthDelim outer: %v", err)
	}
	if r.HasMore() {
		t.Fatal("trailing bytes after decode")
	}
}

// TestMarshalToWriterPeakAllocBelowRaw pins the load-bearing property of
// the streaming compressed path: the raw body never lands in any single
// buffer. With the fixture below the raw body is several MB while the
// streaming path's incremental allocations stay well under that — most
// of the bytes flow into the zstd encoder a streamThreshold-chunk at a
// time and never accumulate as a single []byte we own.
//
// Fixture choice: few-but-large length-delim regions. The size pass's
// recordedRegions slice is O(num_regions × 8 bytes), so a fixture with
// 400k tiny regions inflates the size-pass overhead to roughly the raw-
// body size and masks the streaming win. The realistic shape — outer
// region + a small number of large inner regions — keeps the metadata
// footprint negligible relative to the raw body, which is what the
// streaming guarantee is supposed to capture.
//
// We prime the encoder pool (multiple iterations) so the measured run
// sees a warm encoder. We deliberately do NOT runtime.GC() before
// measurement: sync.Pool's victim-cache eviction on GC would force the
// measured call to re-allocate the encoder's hash/match tables, which
// dominate any plausible raw-body budget.
func TestMarshalToWriterPeakAllocBelowRaw(t *testing.T) {
	// Build a payload that compresses well but isn't all-zero (so we
	// exercise real entropy-coding paths inside zstd). 50 KiB of repeated
	// "GSBM streaming compression test " is ~1600 bytes raw entropy that
	// compresses to a few hundred bytes per chunk.
	const oneChunk = 50 * 1024
	chunkBuf := make([]byte, 0, oneChunk)
	for len(chunkBuf) < oneChunk {
		chunkBuf = append(chunkBuf, "GSBM streaming compression test "...)
	}
	chunkBuf = chunkBuf[:oneChunk]
	payload := string(chunkBuf)

	// numRegions chosen so raw ≈ 10 MiB. Larger than zstd's first-touch
	// encoder construction cost (~1–10 MiB for hash/match tables under
	// SpeedFastest); without this margin the test goes flaky in a full
	// `go test ./...` run where prior tests have churned through the
	// pool and the measured call may hit pool.New().
	const numRegions = 200
	m := &fewLargeRegionsMarshaler{payloads: make([]string, numRegions)}
	for i := range m.payloads {
		m.payloads[i] = payload
	}

	// Probe the raw body size via the size pass so the assertion's bound
	// is a real number rather than a guess.
	sw := newRecordingSizeWriter()
	if err := m.MarshalGSBM(sw); err != nil {
		t.Fatalf("size pass: %v", err)
	}
	rawBodySize := sw.Size()
	const zstdBlockBytes = 128 * 1024
	if rawBodySize < zstdBlockBytes*4 {
		t.Fatalf("fixture too small: rawBodySize=%d, want >> %d (zstd block)", rawBodySize, zstdBlockBytes)
	}

	// Prime the encoder pool so the first call's lazy initialization is
	// not attributed to the measured run.
	for i := range 4 {
		if err := MarshalToWriter(io.Discard, m, 0, Options{Compress: true}); err != nil {
			t.Fatalf("primer call %d: %v", i, err)
		}
	}

	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)

	if err := MarshalToWriter(io.Discard, m, 0, Options{Compress: true}); err != nil {
		t.Fatalf("MarshalToWriter: %v", err)
	}

	runtime.ReadMemStats(&after)

	allocated := after.TotalAlloc - before.TotalAlloc
	if allocated >= uint64(rawBodySize) {
		t.Fatalf("streaming path allocated %d bytes, raw body is %d bytes — raw body likely materialized as a single buffer (peak alloc must be < rawBodySize)", allocated, rawBodySize)
	}
	t.Logf("streaming alloc=%d B, raw=%d B (ratio %.3f)", allocated, rawBodySize, float64(allocated)/float64(rawBodySize))
}

// fewLargeRegionsMarshaler emits one outer length-delim region wrapping
// len(payloads) inner length-delim regions, each carrying one of the
// caller-supplied strings as a tagged string field. Designed for the
// peak-alloc test where the streaming win is dominated by avoiding
// concatenation of large per-region payloads — keep the region count
// small and individual region size large.
type fewLargeRegionsMarshaler struct {
	payloads []string
}

func (m *fewLargeRegionsMarshaler) SizeGSBM() int {
	cw := NewCountingWriter()
	_ = m.MarshalGSBM(cw)
	return cw.Size()
}

func (m *fewLargeRegionsMarshaler) MarshalGSBM(w *Writer) error {
	w.WriteTag(1, WireLengthDelim)
	outer := w.BeginLengthDelim()
	w.WriteUvarint(uint64(len(m.payloads)))
	for _, p := range m.payloads {
		inner := w.BeginLengthDelim()
		w.WriteTag(1, WireLengthDelim)
		w.WriteString(p)
		w.EndLengthDelim(inner)
	}
	w.EndLengthDelim(outer)
	return w.Err()
}

// TestMarshalToWriterPropagatesUserWriteError checks that an io.Writer
// failure from w bubbles back to the caller instead of being swallowed.
// Without this guard a write-error on a network connection would result
// in a silently-truncated blob on the wire.
func TestMarshalToWriterPropagatesUserWriteError(t *testing.T) {
	sentinel := errors.New("network down")
	failOn := &failingWriter{failAt: 0, err: sentinel}
	err := MarshalToWriter(failOn, pinnedMarshaler{}, 0, Options{Compress: true})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err=%v, want %v", err, sentinel)
	}
	err = MarshalToWriter(failOn, pinnedMarshaler{}, 0, Options{})
	if !errors.Is(err, sentinel) {
		t.Fatalf("uncompressed err=%v, want %v", err, sentinel)
	}
}

// TestRecordingSizeWriterRegionOrder pins the ordering invariant the
// streaming path relies on: recorded region sizes are appended in
// BeginLengthDelim order (outer first, then nested), NOT
// EndLengthDelim order (inner first). The streaming write pass
// consumes the recorded list in BeginLengthDelim order, so a swap
// would silently emit wrong canonical varint lengths.
func TestRecordingSizeWriterRegionOrder(t *testing.T) {
	sw := newRecordingSizeWriter()
	// outer region with two sibling inner regions inside it.
	outer := sw.BeginLengthDelim()
	inner1 := sw.BeginLengthDelim()
	sw.WriteString("aaaa") // 1 (len varint) + 4 = 5 bytes
	sw.EndLengthDelim(inner1)
	inner2 := sw.BeginLengthDelim()
	sw.WriteString("bb") // 1 + 2 = 3 bytes
	sw.EndLengthDelim(inner2)
	sw.EndLengthDelim(outer)

	sizes := sw.recordedRegionSizes()
	if len(sizes) != 3 {
		t.Fatalf("recorded %d regions, want 3: %v", len(sizes), sizes)
	}
	// Outer must come first (it was opened first), then the two inner
	// siblings in declaration order. The "EndLengthDelim order" bug
	// would give us [4, 2, outer] instead of [outer, 4, 2].
	//
	// Outer body covers everything between its Begin/End: each inner
	// contributes a 1-byte canonical len varint + its body bytes —
	// (1+4) + (1+2) = 8. Inner1 body is "aaaa" preceded by a 1-byte len
	// varint = 5 bytes total, but recorded size is the inner BODY only
	// (the bytes after the inner length varint), which is the 5-byte
	// WriteString payload — len varint (1) + chars (4) = 5.
	if sizes[0] != 10 {
		t.Fatalf("recorded[0] (outer body) = %d, want 10", sizes[0])
	}
	if sizes[1] != 5 {
		t.Fatalf("recorded[1] (inner1 body) = %d, want 5", sizes[1])
	}
	if sizes[2] != 3 {
		t.Fatalf("recorded[2] (inner2 body) = %d, want 3", sizes[2])
	}
}

// TestStreamingEncoderPoolReuseAfterClose extends the pool-reuse
// guarantee to the streaming code path: many sequential
// MarshalToWriter{Compress:true} calls must reuse the same small set of
// zstd encoders rather than allocating a fresh one each time. enc.Close
// followed by enc.Reset cycles a single instance cleanly per klauspost
// docs; this test pins that contract.
func TestStreamingEncoderPoolReuseAfterClose(t *testing.T) {
	// Borrow once and return without using, so prior tests' encoder is
	// drained from the head of the pool and the identities we observe
	// reflect this test's activity. sync.Pool gives no draining
	// primitive, but one Get/Put cycle is enough in practice to clear
	// the most-recently-pushed entry.
	enc := getEncoder()
	putEncoder(enc)

	seen := make(map[*zstd.Encoder]struct{})
	for i := range 50 {
		var buf bytes.Buffer
		if err := MarshalToWriter(&buf, pinnedMarshaler{}, 0, Options{Compress: true}); err != nil {
			t.Fatalf("iter %d: %v", i, err)
		}
		// Borrow an encoder to inspect identity. Returning it
		// immediately keeps the pool warm for the next iteration.
		enc := getEncoder()
		seen[enc] = struct{}{}
		putEncoder(enc)
	}
	if len(seen) > 8 {
		t.Fatalf("observed %d distinct encoder instances over 50 cycles; pool not reusing across Close/Reset", len(seen))
	}
}

// failingWriter returns sentinel on its first Write call (or after the
// failAt-th, for partial-write paths the streaming compressed flow may
// take). Used to confirm error propagation through the streaming chain.
type failingWriter struct {
	failAt int
	calls  int
	err    error
}

func (f *failingWriter) Write(p []byte) (int, error) {
	if f.calls >= f.failAt {
		return 0, f.err
	}
	f.calls++
	return len(p), nil
}

// nestedRepeatedMarshaler emits an outer length-delim region wrapping n
// inner length-delim regions, each holding a short repeated string and a
// varint. Mirrors the shape of repeatednested.Batch but lives inside the
// gsbm package so writer-level tests don't pull in the bench module.
type nestedRepeatedMarshaler struct {
	n int
}

func (m *nestedRepeatedMarshaler) SizeGSBM() int {
	cw := NewCountingWriter()
	_ = m.MarshalGSBM(cw)
	return cw.Size()
}

func (m *nestedRepeatedMarshaler) MarshalGSBM(w *Writer) error {
	w.WriteTag(1, WireLengthDelim)
	outer := w.BeginLengthDelim()
	w.WriteUvarint(uint64(m.n))
	for i := range m.n {
		inner := w.BeginLengthDelim()
		w.WriteTag(1, WireLengthDelim)
		w.WriteString("code")
		w.WriteTag(2, WireVarint)
		w.WriteUvarint(uint64(i))
		w.EndLengthDelim(inner)
	}
	w.EndLengthDelim(outer)
	return w.Err()
}
