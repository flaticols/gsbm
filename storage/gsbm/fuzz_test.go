package gsbm_test

import (
	"bytes"
	"errors"
	"runtime/debug"
	"testing"

	"go.flaticols.dev/gsbm/internal/bench"
	"go.flaticols.dev/gsbm/storage/gsbm"
	"go.flaticols.dev/gsbm/tools/gsbmcodegen/fixtures/sample"
)

// documentedSentinels is the closed set of error values the gsbm runtime
// wraps around every malformed-input path. The fuzz harnesses assert any
// surfaced error chains back to one of these via errors.Is — a raw
// errors.New string deep in the call stack is a regression because
// callers cannot inspect it.
var documentedSentinels = []error{
	gsbm.ErrBadMagic,
	gsbm.ErrUnsupportedVer,
	gsbm.ErrReservedFlags,
	gsbm.ErrTruncated,
	gsbm.ErrTrailingBytes,
	gsbm.ErrVarintOverflow,
	gsbm.ErrZeroTag,
	gsbm.ErrReservedWire,
	gsbm.ErrWrongWireType,
	gsbm.ErrTagOverflow,
	gsbm.ErrInvalidMapKey,
	gsbm.ErrInvalidPresence,
	gsbm.ErrBodyTooLarge,
	gsbm.ErrIntegerOverflow,
	gsbm.ErrAllocTooLarge,
	gsbm.ErrBodyLenMismatch,
	gsbm.ErrCorruptCompressedBody,
}

func isDocumentedSentinel(err error) bool {
	for _, s := range documentedSentinels {
		if errors.Is(err, s) {
			return true
		}
	}
	return false
}

// largeOrderBlob produces a stable 1-2 MiB wire blob (header + body) used
// as a fuzz seed. Failing the test if generation fails keeps seed wiring
// reproducible across machines.
func largeOrderBlob(tb testing.TB) []byte {
	tb.Helper()
	o := bench.MakeLargeOrder(0, 1<<20, 2<<20)
	bodySize, err := bench.EncodedSize(&o)
	if err != nil {
		tb.Fatalf("EncodedSize: %v", err)
	}
	w := gsbm.NewWriter(nil)
	w.WriteHeader(0, 1, uint32(bodySize))
	if err := o.MarshalGSBM(w); err != nil {
		tb.Fatalf("MarshalGSBM: %v", err)
	}
	if err := w.Err(); err != nil {
		tb.Fatalf("writer err: %v", err)
	}
	return w.Bytes()
}

// FuzzReaderRobustness drives arbitrary bytes through the decoder and
// asserts: (1) no panic escapes from the Reader; (2) any returned error
// chains to a documented gsbm.Err* sentinel via errors.Is; and (3) the
// raw-Reader walk makes forward progress (every successful ReadTag +
// SkipField pair consumes at least one byte) so a malformed blob cannot
// trap the decoder in an infinite loop.
func FuzzReaderRobustness(f *testing.F) {
	f.Add(largeOrderBlob(f))
	// fmtVer=2, flags=0, schemaHint=0, bodyLen=0 — valid empty-body header.
	f.Add([]byte("GSBM\x02\x00\x00\x00\x00\x00\x00\x00"))
	f.Add([]byte{})
	// Single-tag varint after a valid header (tag 1, WireVarint, value 0).
	// Body is 2 bytes, so bodyLen=2.
	f.Add([]byte("GSBM\x02\x00\x00\x00\x02\x00\x00\x00\x08\x00"))
	// A blob whose body bytes are 10 0xFFs (varint overflow). bodyLen=10.
	f.Add([]byte("GSBM\x02\x00\x00\x00\x0a\x00\x00\x00\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff"))
	// Valid gzip- and zstd-compressed blobs so the fuzzer explores the
	// decompress paths (the gzip inflate cap, frame corruption) rather than
	// only rejecting at the header. A zero-value Order marshals to a small
	// valid body that round-trips through each codec.
	{
		var zero sample.Order
		if gz, err := gsbm.MarshalWithOptions(&zero, 1, gsbm.Options{Compression: gsbm.CompressionGzip}); err == nil {
			f.Add(gz)
		}
		if zs, err := gsbm.MarshalWithOptions(&zero, 1, gsbm.Options{Compression: gsbm.CompressionZstd}); err == nil {
			f.Add(zs)
		}
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("panic on %x: %v\n%s", data, r, debug.Stack())
			}
		}()

		// Path 1: full DecodeInto exercise — the production hot path.
		// Generated UnmarshalGSBM uses a stack-local presence bitmap, so
		// no sidecar drain is needed between fuzz execs.
		var dst sample.Order
		err := gsbm.DecodeInto(data, &dst)
		if err != nil && !isDocumentedSentinel(err) {
			t.Fatalf("DecodeInto returned non-sentinel error on %x: %v", data, err)
		}

		// Path 2: raw Reader walk — exercises the primitives directly so a
		// regression in ReadTag / SkipField surfaces independent of codegen.
		r := gsbm.NewReader(data)
		_, _, _, herr := r.ReadHeader()
		if herr != nil {
			if !isDocumentedSentinel(herr) {
				t.Fatalf("ReadHeader returned non-sentinel error on %x: %v", data, herr)
			}
			return
		}
		for r.HasMore() {
			pos := r.Pos()
			_, wt, terr := r.ReadTag()
			if terr != nil {
				if !isDocumentedSentinel(terr) {
					t.Fatalf("ReadTag returned non-sentinel error on %x: %v", data, terr)
				}
				return
			}
			if serr := r.SkipField(wt); serr != nil {
				if !isDocumentedSentinel(serr) {
					t.Fatalf("SkipField returned non-sentinel error on %x: %v", data, serr)
				}
				return
			}
			if r.Pos() <= pos {
				t.Fatalf("forward-progress violation on %x: pos %d -> %d", data, pos, r.Pos())
			}
		}
		if e := r.Err(); e != nil && !isDocumentedSentinel(e) {
			t.Fatalf("Reader sticky err non-sentinel on %x: %v", data, e)
		}
	})
}

// FuzzWriterReaderRoundTripCanonical validates the canonical-form
// invariant: re-encoding a decoded value yields the same bytes as the
// canonical wire form. We assert this idempotently — given input D, we
// compute B1 = encode(decode(D)); then B2 = encode(decode(B1)); and
// require B1 == B2. The idempotent form is what the spec guarantees:
// the original D may be non-canonical (unsorted map keys per spec §5.3,
// non-minimum varints per spec §4.1, last-wins duplicates per spec §3.3,
// or unknown tags forward-skipped via SkipField), so a strict D == B1
// check would flag known design choices rather than real bugs. The
// idempotent check still catches an encoder that drifts away from
// canonical form on every pass.
func FuzzWriterReaderRoundTripCanonical(f *testing.F) {
	f.Add(largeOrderBlob(f))
	f.Add([]byte("GSBM\x02\x00\x00\x00\x00\x00\x00\x00"))
	f.Add([]byte("GSBM\x02\x00\x00\x00\x02\x00\x00\x00\x08\x00"))

	f.Fuzz(func(t *testing.T, data []byte) {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("panic on %x: %v\n%s", data, r, debug.Stack())
			}
		}()

		var first sample.Order
		r := gsbm.NewReader(data)
		flags, schemaHint, _, err := r.ReadHeader()
		if err != nil {
			return
		}
		if err := first.UnmarshalGSBM(r); err != nil {
			return
		}
		if err := r.Err(); err != nil {
			return
		}

		// Re-encode with a placeholder bodyLen, then patch it from the
		// actual body byte count via FinalizeBodyLen.
		w1 := gsbm.NewWriter(nil)
		w1.WriteHeader(flags, schemaHint, 0)
		if err := first.MarshalGSBM(w1); err != nil {
			t.Fatalf("first re-encode failed on %x: %v", data, err)
		}
		if err := w1.Err(); err != nil {
			t.Fatalf("first writer err on %x: %v", data, err)
		}
		w1.FinalizeBodyLen()
		canonical := append([]byte(nil), w1.Bytes()...)

		// Decode the canonical form and re-encode; the second pass must
		// reproduce `canonical` byte-for-byte. If it does not, the encoder
		// is failing to converge on a single canonical representation.
		var second sample.Order
		r2 := gsbm.NewReader(canonical)
		flags2, schemaHint2, _, err := r2.ReadHeader()
		if err != nil {
			t.Fatalf("ReadHeader on canonical re-encode failed: %v", err)
		}
		if flags2 != flags || schemaHint2 != schemaHint {
			t.Fatalf("header drift: flags %#x->%#x schemaHint %#x->%#x",
				flags, flags2, schemaHint, schemaHint2)
		}
		if err := second.UnmarshalGSBM(r2); err != nil {
			t.Fatalf("UnmarshalGSBM on canonical re-encode failed: %v", err)
		}
		if err := r2.Err(); err != nil {
			t.Fatalf("Reader err on canonical re-encode: %v", err)
		}

		w2 := gsbm.NewWriter(nil)
		w2.WriteHeader(flags2, schemaHint2, 0)
		if err := second.MarshalGSBM(w2); err != nil {
			t.Fatalf("second re-encode failed: %v", err)
		}
		if err := w2.Err(); err != nil {
			t.Fatalf("second writer err: %v", err)
		}
		w2.FinalizeBodyLen()
		if !bytes.Equal(w2.Bytes(), canonical) {
			t.Fatalf("non-canonical re-encode (encoder did not converge):\n  pass1: %x\n  pass2: %x", canonical, w2.Bytes())
		}
	})
}

// FuzzHeaderCorruption seeds with a valid 1-2 MiB blob and lets the
// fuzzer replace the 12-byte header. The body is left untouched so the
// failure mode is isolated to header validation: any deviation from the
// magic / fmtVer / flags / bodyLen rules must surface the matching
// sentinel, never a panic, and never decode successfully.
func FuzzHeaderCorruption(f *testing.F) {
	valid := largeOrderBlob(f)
	validBodyLen := uint32(len(valid) - gsbm.HeaderSize)
	hdr := func(magic string, fmtVer, flags byte, schemaHint uint16, bodyLen uint32) []byte {
		out := make([]byte, gsbm.HeaderSize)
		copy(out[:4], magic)
		out[4] = fmtVer
		out[5] = flags
		out[6] = byte(schemaHint)
		out[7] = byte(schemaHint >> 8)
		out[8] = byte(bodyLen)
		out[9] = byte(bodyLen >> 8)
		out[10] = byte(bodyLen >> 16)
		out[11] = byte(bodyLen >> 24)
		return out
	}

	f.Add(hdr("GSBM", 2, 0, 0, validBodyLen))           // canonical header
	f.Add(hdr("XXXX", 2, 0, 0, validBodyLen))           // bad magic
	f.Add(hdr("GSBM", 9, 0, 0, validBodyLen))           // bad fmtVer
	f.Add(hdr("GSBM", 1, 0, 0, validBodyLen))           // legacy fmtVer 1 — must be rejected
	f.Add(hdr("GSBM", 2, 0x01, 0, validBodyLen))        // compressed marker — body is not zstd
	f.Add(hdr("GSBM", 2, 0x02, 0, validBodyLen))        // reserved flag bit set
	f.Add(hdr("GSBM", 2, 0, 0x1234, validBodyLen))      // schemaHint variant (still valid)
	f.Add(hdr("GSBM", 2, 0x80, 0xFFFF, validBodyLen))   // flags+schemaHint mutated
	f.Add(hdr("GSBM", 2, 0, 0, 0))                 // bodyLen = 0 with non-empty body
	f.Add(hdr("GSBM", 2, 0, 0, validBodyLen-1))    // bodyLen off-by-one
	f.Add(hdr("GSBM", 2, 0, 0, validBodyLen+1))    // bodyLen overstated by 1
	f.Add(hdr("GSBM", 2, 0, 0, ^uint32(0)))        // bodyLen = MaxUint32
	f.Add(make([]byte, gsbm.HeaderSize))            // all-zero header

	f.Fuzz(func(t *testing.T, header []byte) {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("panic on header %x: %v\n%s", header, r, debug.Stack())
			}
		}()
		if len(header) < gsbm.HeaderSize {
			return
		}
		header = header[:gsbm.HeaderSize]

		blob := make([]byte, len(valid))
		copy(blob, valid)
		copy(blob[:gsbm.HeaderSize], header)

		var dst sample.Order
		err := gsbm.DecodeInto(blob, &dst)

		magicOK := bytes.Equal(header[:4], []byte(gsbm.Magic))
		verOK := header[4] == gsbm.FmtVer2
		// Bits 0–2 are the compression method (0 none / 1 zstd / 2 gzip);
		// bits 3–7 are reserved. Reject a reserved high bit or an unknown
		// method value (3–7); accept the three defined methods.
		method := header[5] & 0x07
		highBitsSet := header[5]&0xF8 != 0
		methodKnown := method == uint8(gsbm.CompressionNone) ||
			method == uint8(gsbm.CompressionZstd) ||
			method == uint8(gsbm.CompressionGzip)
		flagsOK := !highBitsSet && methodKnown
		compressed := flagsOK && method != uint8(gsbm.CompressionNone)
		bodyLen := uint32(header[8]) | uint32(header[9])<<8 |
			uint32(header[10])<<16 | uint32(header[11])<<24
		bodyLenOK := uint64(bodyLen) == uint64(len(blob)-gsbm.HeaderSize)

		switch {
		case !magicOK:
			if !errors.Is(err, gsbm.ErrBadMagic) {
				t.Fatalf("header %x: want ErrBadMagic, got %v", header, err)
			}
		case !verOK:
			if !errors.Is(err, gsbm.ErrUnsupportedVer) {
				t.Fatalf("header %x: want ErrUnsupportedVer, got %v", header, err)
			}
		case !flagsOK:
			if !errors.Is(err, gsbm.ErrReservedFlags) {
				t.Fatalf("header %x: want ErrReservedFlags, got %v", header, err)
			}
		case !bodyLenOK:
			if !errors.Is(err, gsbm.ErrBodyLenMismatch) {
				t.Fatalf("header %x: want ErrBodyLenMismatch, got %v", header, err)
			}
		case compressed:
			// A compressing method (zstd or gzip) + valid bodyLen, but the
			// seed body is a canonical uncompressed sample.Order — it will
			// not parse as a zstd or gzip frame. The framing layer must
			// surface that as a clean ErrCorruptCompressedBody (no panic,
			// no silent success).
			if !errors.Is(err, gsbm.ErrCorruptCompressedBody) {
				t.Fatalf("header %x with non-compressed body: want ErrCorruptCompressedBody, got %v", header, err)
			}
		default:
			// Magic + fmtVer + flags + bodyLen all valid; header[6:8] is
			// schemaHint and is informational. Body is unchanged from the
			// seed, so decode must succeed.
			if err != nil {
				t.Fatalf("valid header %x with intact body decoded with err %v", header, err)
			}
		}
	})
}
