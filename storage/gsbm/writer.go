package gsbm

import (
	"encoding/binary"
	"math"
)

// reservedLenBytes is the slot size reserved for a length-delimited region's
// length varint. 5 bytes covers payloads up to (1<<35)-1, which exceeds any
// realistic blob.
const reservedLenBytes = 5

// Writer appends a tagged-binary blob to a caller-owned byte slice. The
// zero value is not usable; construct with NewWriter or Reset on a pooled
// instance. A Writer is not safe for concurrent use.
//
// A Writer constructed by NewCountingWriter runs in size-only mode: every
// Write* method accumulates the byte count it would have produced into
// sizeAcc instead of touching buf. Size-mode lets hand-written
// MarshalGSBM implementations measure their exact body size without
// double-walking the field list; the codegen-generated SizeGSBM remains
// the preferred path for generated types.
type Writer struct {
	buf      []byte
	err      error
	sizeOnly bool
	sizeAcc  int
	// scratch caches per-callsite materialization output so that
	// materializing codecs (DecimalString, JSON, …) run their gen
	// function exactly once per gsbm.Marshal call. The same map is
	// threaded across the size→write pass hand-off via adoptScratch:
	// the size-mode Writer populates the cache, the real-mode Writer
	// adopts it (resetting per-entry read cursors), and the second
	// pass hits the cache instead of re-materializing. Lazily
	// allocated on first cached write.
	//
	// A single callsite id may be visited multiple times in one pass
	// — slice and map element loops call MarshalGSBM (and through it
	// the same WriteCachedString call) once per element, all sharing
	// the codegen-emitted <struct,tag> constant. Entries therefore
	// store occurrences in walk order; per-lane cursors advance on
	// each hit so distinct occurrences read their own materialization
	// rather than aliasing to the first one.
	scratch map[uint64]*scratchEntry
}

// scratchEntry holds one callsite's per-occurrence materialization
// outputs. The size pass appends one entry per visit; adoptScratch
// rewinds the per-lane cursors so the write pass re-walks the same
// occurrences in the same order. The string and bytes lanes carry
// independent cursors so that a handwritten codec which mixes
// WriteCachedString with WriteCachedBytes/WriteCachedAppendBytes at
// the same callsite still replays each lane in order.
type scratchEntry struct {
	values     [][]byte // used by WriteCachedBytes (and WriteCachedAppendBytes)
	strings    []string // used by WriteCachedString
	stringsIdx int      // cursor for the strings lane
	valuesIdx  int      // cursor for the values lane
}

// NewWriter wraps buf for appending. The caller retains ownership; the
// Writer never reallocates beyond what append on the slice does.
func NewWriter(buf []byte) *Writer {
	return &Writer{buf: buf}
}

// NewCountingWriter returns a Writer in size-only mode: every Write*
// method advances an internal counter instead of appending to a buffer,
// and Size() reports the accumulated byte total. The buffer is never
// allocated; Bytes() returns nil. Size-mode is fixed at construction —
// passing the returned writer to MarshalGSBM is the only intended use.
func NewCountingWriter() *Writer {
	return &Writer{sizeOnly: true}
}

// Bytes returns the accumulated output. The caller MUST treat the result
// as read-only until the Writer is reset or discarded. For a size-mode
// Writer (NewCountingWriter), Bytes returns nil — there is no buffer.
func (w *Writer) Bytes() []byte { return w.buf }

// Size reports the accumulated body byte count produced by a size-mode
// Writer. Zero for a real-mode Writer (NewWriter). Includes the 12-byte
// header iff WriteHeader was called.
func (w *Writer) Size() int { return w.sizeAcc }

// Reset re-points the Writer at buf (truncated to length 0) and clears the
// sticky error. Callers pool a Writer and call Reset(b[:0]) per request.
// Reset does not change the size-mode flag; pool a size-mode and a
// real-mode Writer separately.
func (w *Writer) Reset(buf []byte) {
	w.buf = buf[:0]
	w.err = nil
	w.sizeAcc = 0
	clear(w.scratch)
}

// Err returns the first error captured during writing, or nil. Once an
// error is set, subsequent writes are no-ops.
func (w *Writer) Err() error { return w.err }

func (w *Writer) setErr(err error) {
	if w.err == nil {
		w.err = err
	}
}

// WriteHeader emits the 12-byte blob header (magic, fmtVer=2, flags,
// schemaHint, bodyLen). Per spec, a writer SHOULD call WriteHeader exactly
// once, before the body. bodyLen MUST equal the byte count of the body that
// will follow (the body bytes appended to this Writer between WriteHeader
// and Bytes()). Decoders verify the cross-check.
func (w *Writer) WriteHeader(flags uint8, schemaHint uint16, bodyLen uint32) {
	if w.err != nil {
		return
	}
	if w.sizeOnly {
		w.sizeAcc += HeaderSize
		return
	}
	w.buf = append(w.buf,
		Magic[0], Magic[1], Magic[2], Magic[3],
		FmtVer2, flags,
		0, 0, // schemaHint placeholder, patched below
		0, 0, 0, 0, // bodyLen placeholder, patched below
	)
	binary.LittleEndian.PutUint16(w.buf[len(w.buf)-6:len(w.buf)-4], schemaHint)
	binary.LittleEndian.PutUint32(w.buf[len(w.buf)-4:], bodyLen)
}

// FinalizeBodyLen patches the header's bodyLen field with the byte count
// of the body buffered after the header. Callers MUST have called
// WriteHeader first; the buffer's first HeaderSize bytes are assumed to
// hold the header. Intended for paths where the body size was unknown at
// WriteHeader time and a placeholder was passed. Production paths that
// already have a Sizer (e.g., gsbm.Marshal) pass bodyLen to WriteHeader
// directly and have no need to call this.
//
// Sets ErrBodyTooLarge if the buffered body exceeds math.MaxUint32 bytes;
// the header bodyLen is a uint32 and silent truncation would produce a
// blob the reader rejects only later as ErrBodyLenMismatch.
func (w *Writer) FinalizeBodyLen() {
	if w.err != nil || w.sizeOnly || len(w.buf) < HeaderSize {
		return
	}
	bodyLen := len(w.buf) - HeaderSize
	if uint64(bodyLen) > math.MaxUint32 {
		w.setErr(ErrBodyTooLarge)
		return
	}
	binary.LittleEndian.PutUint32(w.buf[8:12], uint32(bodyLen))
}

// WriteTag emits the field key (tag<<3 | wireType) as a varint. tag must
// be in [1, MaxTag]; wt must be a defined wire type for fmtVer 2.
func (w *Writer) WriteTag(tag uint32, wt WireType) {
	if w.err != nil {
		return
	}
	if tag == 0 {
		w.setErr(ErrZeroTag)
		return
	}
	if tag > MaxTag {
		w.setErr(ErrTagOverflow)
		return
	}
	if !validWireType(wt) {
		w.setErr(ErrReservedWire)
		return
	}
	if w.sizeOnly {
		w.sizeAcc += varintLen((uint64(tag) << 3) | uint64(wt))
		return
	}
	w.buf = appendUvarint(w.buf, (uint64(tag)<<3)|uint64(wt))
}

// WriteUvarint writes an unsigned varint value (no key).
func (w *Writer) WriteUvarint(v uint64) {
	if w.err != nil {
		return
	}
	if w.sizeOnly {
		w.sizeAcc += varintLen(v)
		return
	}
	w.buf = appendUvarint(w.buf, v)
}

// WriteVarint writes a signed integer using zigzag encoding.
func (w *Writer) WriteVarint(v int64) {
	if w.err != nil {
		return
	}
	if w.sizeOnly {
		w.sizeAcc += varintLen(zigzagEncode64(v))
		return
	}
	w.buf = appendUvarint(w.buf, zigzagEncode64(v))
}

// WriteBool writes a boolean as varint 0 or 1.
func (w *Writer) WriteBool(b bool) {
	if w.err != nil {
		return
	}
	if w.sizeOnly {
		w.sizeAcc++
		return
	}
	if b {
		w.buf = append(w.buf, 1)
	} else {
		w.buf = append(w.buf, 0)
	}
}

// WriteFixed32 writes a 4-byte little-endian value (used for float32 bits
// or fixed-width 32-bit integers when chosen by the schema).
func (w *Writer) WriteFixed32(v uint32) {
	if w.err != nil {
		return
	}
	if w.sizeOnly {
		w.sizeAcc += 4
		return
	}
	var b [4]byte
	binary.LittleEndian.PutUint32(b[:], v)
	w.buf = append(w.buf, b[:]...)
}

// WriteFixed64 writes an 8-byte little-endian value.
func (w *Writer) WriteFixed64(v uint64) {
	if w.err != nil {
		return
	}
	if w.sizeOnly {
		w.sizeAcc += 8
		return
	}
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], v)
	w.buf = append(w.buf, b[:]...)
}

// WriteFloat32 writes the IEEE 754 LE bits of f.
func (w *Writer) WriteFloat32(f float32) { w.WriteFixed32(math.Float32bits(f)) }

// WriteFloat64 writes the IEEE 754 LE bits of f.
func (w *Writer) WriteFloat64(f float64) { w.WriteFixed64(math.Float64bits(f)) }

// WriteString writes a varint length followed by the string bytes.
func (w *Writer) WriteString(s string) {
	if w.err != nil {
		return
	}
	if w.sizeOnly {
		w.sizeAcc += varintLen(uint64(len(s))) + len(s)
		return
	}
	w.buf = appendUvarint(w.buf, uint64(len(s)))
	w.buf = append(w.buf, s...)
}

// WriteBytes writes a varint length followed by the byte payload. The
// caller's slice is copied into the Writer's buffer.
func (w *Writer) WriteBytes(p []byte) {
	if w.err != nil {
		return
	}
	if w.sizeOnly {
		w.sizeAcc += varintLen(uint64(len(p))) + len(p)
		return
	}
	w.buf = appendUvarint(w.buf, uint64(len(p)))
	w.buf = append(w.buf, p...)
}

// BeginLengthDelim opens a length-prefixed region (nested struct, slice
// body, or map body). It reserves space for the length varint and returns
// a marker that must be passed to EndLengthDelim once the body is written.
//
// In size-mode, no reservation happens; the returned marker is the
// sizeAcc snapshot at the call site, and EndLengthDelim emits the
// SizeUvarint(bodyLen) for the length prefix at close.
func (w *Writer) BeginLengthDelim() int {
	if w.err != nil {
		return 0
	}
	if w.sizeOnly {
		return w.sizeAcc
	}
	pos := len(w.buf)
	// Reserve the maximum varint footprint we expect for a length value.
	// The tail will be shifted left if the actual encoding is shorter.
	w.buf = append(w.buf, 0, 0, 0, 0, 0)
	return pos
}

// EndLengthDelim closes the region started at marker, patching its length
// prefix and shifting the body left if the encoded length is shorter than
// the reserved slot.
func (w *Writer) EndLengthDelim(marker int) {
	if w.err != nil {
		return
	}
	if w.sizeOnly {
		bodyLen := w.sizeAcc - marker
		if bodyLen < 0 {
			w.setErr(ErrTruncated)
			return
		}
		w.sizeAcc += varintLen(uint64(bodyLen))
		return
	}
	bodyLen := len(w.buf) - marker - reservedLenBytes
	if bodyLen < 0 {
		w.setErr(ErrTruncated)
		return
	}
	n := varintLen(uint64(bodyLen))
	if n > reservedLenBytes {
		// A length varint wider than reservedLenBytes would overrun the
		// reserved slot and corrupt the first body byte. Practically only
		// reachable for ≥2^35-byte bodies, but we refuse to silently
		// produce an unparseable blob.
		w.setErr(ErrBodyTooLarge)
		return
	}
	if n < reservedLenBytes {
		// Shift the body bytes left so the length varint sits flush.
		copy(w.buf[marker+n:], w.buf[marker+reservedLenBytes:])
		w.buf = w.buf[:len(w.buf)-(reservedLenBytes-n)]
	}
	putUvarint(w.buf[marker:marker+n], uint64(bodyLen))
}

// WriteCachedString writes a string-valued materializing-codec body
// with a per-occurrence cache. The first time MarshalGSBM visits a
// given callsite the entry's string list is empty, gen runs, and its
// result is appended at the current stringsIdx; subsequent occurrences
// in the same pass each materialize fresh (so distinct slice/map
// elements at the same codegen-emitted callsite do not alias one
// another). After adoptScratch rewinds the cursors, the write pass
// re-walks each occurrence in the same order and hits the cached
// entry instead of re-materializing — gsbm.Marshal's two-pass flow
// thereby materializes every occurrence exactly once.
//
// The wire shape matches WriteString (varint length followed by the
// bytes). Pass and write traversal order is dictated by MarshalGSBM;
// both passes run the same body, so occurrence order is identical by
// construction (slice element order is preserved; map iteration is
// already sorted by emitMapEncode).
func (w *Writer) WriteCachedString(callsite uint64, gen func() string) error {
	if w.err != nil {
		return w.err
	}
	e := w.entryFor(callsite)
	var s string
	if e.stringsIdx < len(e.strings) {
		s = e.strings[e.stringsIdx]
	} else {
		s = gen()
		e.strings = append(e.strings, s)
	}
	e.stringsIdx++
	w.WriteString(s)
	return w.err
}

// WriteCachedBytes is the byte-returning counterpart to
// WriteCachedString for codecs whose materialization produces []byte
// directly (JSON, gzip, any byte-oriented canonicalization). Each
// occurrence's slice is retained by the cache for the duration of the
// encode call; gen MUST return a slice the Writer is free to hold for
// that lifetime.
func (w *Writer) WriteCachedBytes(callsite uint64, gen func() []byte) error {
	if w.err != nil {
		return w.err
	}
	e := w.entryFor(callsite)
	var b []byte
	if e.valuesIdx < len(e.values) {
		b = e.values[e.valuesIdx]
	} else {
		b = gen()
		e.values = append(e.values, b)
	}
	e.valuesIdx++
	w.WriteBytes(b)
	return w.err
}

// WriteCachedAppendBytes is the append-style counterpart to
// WriteCachedBytes for codecs whose source type supports append-style
// text encoding (encoding.TextAppender, (*big.Int).Append, custom
// decimal types with AppendText, time.Time.AppendFormat, …). On first
// occurrence at a callsite, appendFn is invoked with a nil destination
// and its returned slice is stored in the cache; subsequent occurrences
// at the same callsite read the cached slice. The retained slice's
// lifetime matches the encode call — appendFn must return a slice the
// Writer is free to hold for that duration (returning the scratch
// argument extended via append is the idiomatic shape).
//
// Compared to WriteCachedString, this skips the intermediate string
// allocation entirely: the appended bytes go straight from the source
// type's append method into the cache. Use this when the source value's
// canonical text form is reached via an Append-style method; use
// WriteCachedString when the source only exposes a String()-style API;
// use WriteCachedBytes when the materialization is already a []byte
// (JSON, gzip, etc.).
//
// If appendFn returns a non-nil error, the Writer's sticky error state
// is set and that error is returned; the cache is not advanced and no
// bytes are written.
func (w *Writer) WriteCachedAppendBytes(callsite uint64, appendFn func(dst []byte) ([]byte, error)) error {
	if w.err != nil {
		return w.err
	}
	e := w.entryFor(callsite)
	var b []byte
	if e.valuesIdx < len(e.values) {
		b = e.values[e.valuesIdx]
	} else {
		var err error
		b, err = appendFn(nil)
		if err != nil {
			w.setErr(err)
			return err
		}
		e.values = append(e.values, b)
	}
	e.valuesIdx++
	w.WriteBytes(b)
	return w.err
}

func (w *Writer) entryFor(callsite uint64) *scratchEntry {
	if w.scratch == nil {
		w.scratch = make(map[uint64]*scratchEntry)
	}
	e := w.scratch[callsite]
	if e == nil {
		e = &scratchEntry{}
		w.scratch[callsite] = e
	}
	return e
}

// adoptScratch moves other's materialization cache into w and rewinds
// every entry's per-lane cursors so the write pass re-reads occurrences
// in their original order. After the call, w hits the cached entries and
// other's cache is cleared. Intended for gsbm.Marshal's hand-off from
// the size-mode Writer to the real-mode Writer; outside that flow
// callers have no reason to invoke it. Safe to call when other has no
// cache (no-op).
func (w *Writer) adoptScratch(other *Writer) {
	if other == nil {
		return
	}
	w.scratch = other.scratch
	for _, e := range w.scratch {
		e.stringsIdx = 0
		e.valuesIdx = 0
	}
	other.scratch = nil
}
